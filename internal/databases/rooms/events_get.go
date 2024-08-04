package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Check if we have an event (that we accepted)
func (r *RoomsDatabase) DoesEventExist(ctx context.Context, eventID id.EventID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		key := r.events.KeyForIDToVersion(eventID)
		if b, err := txn.Get(key).Get(); err != nil {
			return false, err
		} else if b == nil {
			return false, nil
		} else {
			return true, nil
		}
	})
}

func (r *RoomsDatabase) MustDoesEventExist(ctx context.Context, eventID id.EventID) bool {
	if ret, err := r.DoesEventExist(ctx, eventID); err != nil {
		panic(err)
	} else {
		return ret
	}
}

func (r *RoomsDatabase) GetEvent(ctx context.Context, eventID id.EventID) (*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Event, error) {
		key := r.events.KeyForEvent(eventID)
		if b, err := txn.Get(key).Get(); err != nil {
			return nil, err
		} else if b == nil {
			return nil, nil
		} else {
			return types.MustNewEventFromBytes(b, eventID), nil
		}
	})
}

// Get the auth chain for a given event by fetching it's auth events and their auth events, etc recursively
func (r *RoomsDatabase) GetEventAuthChain(ctx context.Context, eventID id.EventID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		reqEv, err := eventsProvider.Get(eventID)
		if err != nil {
			return nil, err
		}
		evs, err := r.events.TxnGetAuthChainForEvents(txn, []*types.Event{reqEv}, eventsProvider)
		if err != nil {
			return nil, err
		}
		util.SortEventList(evs)
		return evs, nil
	})
}

func (r *RoomsDatabase) GetRoomStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupRoomStateEventIDsAtEvent(txn, roomID, eventID, nil)
	})
}

func (r *RoomsDatabase) GetRoomAuthStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupRoomAuthStateMapAtEvent(ctx, txn, roomID, eventID, nil)
	})
}

func (r *RoomsDatabase) GetRoomSpecificRoomMemberStateMapAtEvent(ctx context.Context, roomID id.RoomID, userIDs []id.UserID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupSpecificRoomMemberStateMapAtEvent(ctx, txn, roomID, userIDs, eventID, nil)
	})
}

func (r *RoomsDatabase) GetCurrentRoomMemberEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	if memberEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		memberMap, err := r.events.TxnLookupCurrentRoomMemberStateMap(txn, roomID, eventsProvider)
		if err != nil {
			return nil, err
		}
		evs := make([]*types.Event, 0, len(memberMap))
		for _, evID := range memberMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		return evs, nil
	}); err != nil {
		return nil, err
	} else {
		util.SortEventList(memberEvs)
		return memberEvs, nil
	}
}

func (r *RoomsDatabase) GetCurrentRoomStateEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	stateEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap, err := r.events.TxnLookupCurrentRoomStateMap(txn, roomID, eventsProvider)
		if err != nil {
			return nil, err
		}
		evs := make([]*types.Event, 0, len(stateMap))
		for _, evID := range stateMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		return evs, nil
	})
	if err != nil {
		return nil, err
	}

	memberEvs, err := r.GetCurrentRoomMemberEvents(ctx, roomID)
	if err != nil {
		return nil, err
	}

	stateEvs = append(stateEvs, memberEvs...)
	util.SortEventList(stateEvs)
	return stateEvs, nil
}

func (r *RoomsDatabase) GetCurrentRoomServers(ctx context.Context, roomID id.RoomID) ([]string, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]string, error) {
		return r.events.TxnLookupCurrentRoomServers(txn, roomID)
	})
}

func (r *RoomsDatabase) GetCurrentRoomInviteStateEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap, err := r.events.TxnLookupCurrentRoomInviteStateMap(txn, roomID, eventsProvider)
		if err != nil {
			return nil, err
		}
		evs := make([]*types.Event, 0, len(stateMap))
		for _, evID := range stateMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		util.SortEventList(evs)
		return evs, nil
	})
}

type roomStateAtEvent struct {
	StateEvents []*types.Event
	AuthChain   []*types.Event
}

// Get room state (state events + their auth chain) at a given event. This is broken
// down into multuple read transactions so we don't hit the 5s limit. Since events
// are immutable we don't need the consistency guarantees of a single transaction.
func (r *RoomsDatabase) GetRoomStateWithAuthChainAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (roomStateAtEvent, error) {
	var res roomStateAtEvent

	if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap, err := r.events.TxnLookupRoomStateEventIDsAtEvent(txn, roomID, eventID, eventsProvider)
		if err != nil {
			return nil, err
		}
		res.StateEvents = make([]*types.Event, 0, len(stateMap))
		for _, eventID := range stateMap {
			res.StateEvents = append(res.StateEvents, eventsProvider.MustGet(eventID))
		}
		return nil, nil
	}); err != nil {
		return res, err
	}

	if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn).WithEvents(res.StateEvents...)
		if authChain, err := r.events.TxnGetAuthChainForEvents(txn, res.StateEvents, eventsProvider); err != nil {
			return nil, err
		} else {
			res.AuthChain = authChain
		}
		return nil, nil
	}); err != nil {
		return res, err
	}

	util.SortEventList(res.StateEvents)
	util.SortEventList(res.AuthChain)
	return res, nil
}

type roomStateIDsAtEvent struct {
	StateEventIDs []id.EventID
	AuthChainIDs  []id.EventID
}

func (r *RoomsDatabase) GetRoomStateWithAuthChainIDsAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (roomStateIDsAtEvent, error) {
	var res roomStateIDsAtEvent

	roomStateAtEvent, err := r.GetRoomStateWithAuthChainAtEvent(ctx, roomID, eventID)
	if err != nil {
		return res, err
	}

	res.StateEventIDs = make([]id.EventID, 0, len(roomStateAtEvent.StateEvents))
	for _, ev := range roomStateAtEvent.StateEvents {
		res.StateEventIDs = append(res.StateEventIDs, ev.ID)
	}
	res.AuthChainIDs = make([]id.EventID, 0, len(roomStateAtEvent.AuthChain))
	for _, ev := range roomStateAtEvent.AuthChain {
		res.AuthChainIDs = append(res.AuthChainIDs, ev.ID)
	}

	return res, nil
}
