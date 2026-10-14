package rooms

import (
	"context"
	"maps"
	"slices"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Returns those of the events not stored as room events, which includes outliers
func (r *RoomsDatabase) GetUnstoredEventIDs(ctx context.Context, eventIDs []id.EventID) ([]id.EventID, error) {
	stored, err := r.storedEventIDs(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(eventIDs), func(eventID id.EventID) bool {
		_, found := stored[eventID]
		return found
	}), nil
}

// storedEventIDs returns a map of event IDs already stored
func (r *RoomsDatabase) storedEventIDs(ctx context.Context, eventIDs []id.EventID) (map[id.EventID]struct{}, error) {
	stored := make(map[id.EventID]struct{})
	for chunk := range slices.Chunk(eventIDs, storedCheckChunk) {
		chunkStored, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (map[id.EventID]struct{}, error) {
			return r.txnGetStoredEventIDs(txn, chunk)
		})
		if err != nil {
			return nil, err
		}
		maps.Copy(stored, chunkStored)
	}
	return stored, nil
}

// MissingEventsRequest is what a get_missing_events request for a federated batch names
type MissingEventsRequest struct {
	// The prev events neither in the batch nor stored as room events that the batch events not
	// stored yet cite, sorted
	Missing []id.EventID
	// The room's extremities, where the other server stops looking
	Extremities []id.EventID
	// The depth of the room's first timeline event, which after a remote join is the join, zero for
	// a room without one
	MinDepth int64
}

// GetMissingEventsRequest returns what a get_missing_events request for a federated batch names
func (r *RoomsDatabase) GetMissingEventsRequest(ctx context.Context, roomID id.RoomID, batch []*types.Event) (*MissingEventsRequest, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*MissingEventsRequest, error) {
		prevIDs, storedPrevs, err := r.txnOutsidePrevEvents(txn, batch, batch)
		if err != nil {
			return nil, err
		}
		req := &MissingEventsRequest{
			Missing: slices.DeleteFunc(prevIDs, func(prevID id.EventID) bool {
				_, found := storedPrevs[prevID]
				return found
			}),
		}
		if len(req.Missing) == 0 {
			return req, nil
		}
		req.Extremities = r.events.TxnLookupCurrentRoomExtremEventIDs(txn, roomID)
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		if tups := r.events.TxnPaginateRoomEventTups(txn, roomID, types.PaginationOptions{Limit: 1}, eventsProvider); len(tups) > 0 {
			first, err := eventsProvider.Get(tups[0].EventID)
			if err != nil {
				return nil, err
			} else if first != nil {
				req.MinDepth = first.Depth
			}
		}
		return req, nil
	})
}

// Returns the prev events of the pulled events of a federated batch whose state is not known here,
// sorted: those neither in the batch nor stored as room events, and those stored without state.
// Events already stored are not evaluated again, so their prev events are skipped.
func (r *RoomsDatabase) GetPrevEventsWithoutState(
	ctx context.Context,
	roomID id.RoomID,
	batch, pulled []*types.Event,
) ([]id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.EventID, error) {
		return r.txnPrevEventsWithoutState(ctx, txn, roomID, batch, pulled)
	})
}

func (r *RoomsDatabase) txnPrevEventsWithoutState(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	batch, pulled []*types.Event,
) ([]id.EventID, error) {
	prevIDs, storedPrevs, err := r.txnOutsidePrevEvents(txn, batch, pulled)
	if err != nil {
		return nil, err
	}
	eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
	for prevID := range storedPrevs {
		eventsProvider.WillGet(prevID)
	}
	withoutState := make([]id.EventID, 0, len(prevIDs))
	for _, prevID := range prevIDs {
		if _, found := storedPrevs[prevID]; !found {
			withoutState = append(withoutState, prevID)
		} else if prev, err := eventsProvider.Get(prevID); err != nil {
			return nil, err
		} else if !prev.HasStateIn(roomID) {
			withoutState = append(withoutState, prevID)
		}
	}
	return withoutState, nil
}

// Returns the prev events cited from outside a federated batch by those of the events not stored
// yet, sorted, and which of them are stored as room events. Events already stored are not evaluated
// again, so their prev events are skipped.
func (r *RoomsDatabase) txnOutsidePrevEvents(
	txn fdb.ReadTransaction,
	batch, evs []*types.Event,
) ([]id.EventID, map[id.EventID]struct{}, error) {
	storedBatch, err := r.txnGetStoredEventIDs(txn, util.EventsToIDs(batch))
	if err != nil {
		return nil, nil, err
	}
	unstoredBatch := make(map[id.EventID]struct{}, len(batch))
	for _, ev := range batch {
		if _, found := storedBatch[ev.ID]; !found {
			unstoredBatch[ev.ID] = struct{}{}
		}
	}

	var prevIDs []id.EventID
	for _, ev := range evs {
		if _, found := unstoredBatch[ev.ID]; !found {
			continue
		}
		for _, prevID := range ev.PrevEventIDs {
			if _, found := unstoredBatch[prevID]; !found {
				prevIDs = append(prevIDs, prevID)
			}
		}
	}
	slices.Sort(prevIDs)
	prevIDs = slices.Compact(prevIDs)

	storedPrevs, err := r.txnGetStoredEventIDs(txn, prevIDs)
	if err != nil {
		return nil, nil, err
	}
	return prevIDs, storedPrevs, nil
}

func (r *RoomsDatabase) txnGetStoredEventIDs(txn fdb.ReadTransaction, eventIDs []id.EventID) (map[id.EventID]struct{}, error) {
	futures := make([]fdb.FutureByteSlice, len(eventIDs))
	for i, eventID := range eventIDs {
		futures[i] = txn.Get(r.events.KeyForIDToVersion(eventID))
	}
	stored := make(map[id.EventID]struct{}, len(eventIDs))
	for i, future := range futures {
		if b, err := future.Get(); err != nil {
			return nil, err
		} else if b != nil {
			stored[eventIDs[i]] = struct{}{}
		}
	}
	return stored, nil
}

func (r *RoomsDatabase) GetEvent(ctx context.Context, eventID id.EventID) (*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Event, error) {
		return r.events.TxnGetEvent(txn, eventID), nil
	})
}

// Get the auth chain for a given event of the room by fetching it's auth events and their auth events, etc recursively
func (r *RoomsDatabase) GetEventAuthChain(ctx context.Context, roomID id.RoomID, eventID id.EventID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		reqEv, err := eventsProvider.Get(eventID)
		if err != nil {
			return nil, err
		} else if reqEv == nil || reqEv.RoomID != roomID {
			return nil, types.ErrEventNotFound
		}
		evs, err := r.events.TxnGetAuthChainForEvents(txn, []*types.Event{reqEv}, eventsProvider)
		if err != nil {
			return nil, err
		}
		util.SortEventList(evs)
		return evs, nil
	})
}

func (r *RoomsDatabase) PaginateAllEventTups(ctx context.Context, options types.PaginationOptions) ([]types.EventTupWithVersion, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]types.EventTupWithVersion, error) {
		return r.events.TxnPaginateAllEventTups(txn, options, nil), nil
	})
}
