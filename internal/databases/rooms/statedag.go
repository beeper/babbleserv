package rooms

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Until state DAG joins, this server joins no state DAG room (MSC4242) another server hosts, nor
// knocks on one
var ErrStateDAGRemoteJoin = errors.New("joining or knocking on state DAG rooms over federation is not supported yet")

// A state DAG room's events cite only events with state here, so no other server gives states
var errStateDAGGivenStates = errors.New("state DAG rooms take no given states")

// isStateDAGSend reports whether a federated send is to a state DAG room, by the version of the
// room, nil when not stored, or the version the events were verified with
func isStateDAGSend(room *types.Room, evs []*types.Event) bool {
	return room != nil && util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) ||
		slices.ContainsFunc(evs, (*types.Event).StateDAGs)
}

// softFails reports whether step 6 holds an accepted event to the room's current state: in a state
// DAG room only messages soft fail, while state events always join the state DAG
func softFails(ev *types.Event) bool {
	return !ev.StateDAGs() || ev.StateKey == nil
}

// entersStateDAG reports whether a published event of a state DAG room becomes a state extremity:
// an accepted state event does, in place of the prev state events it cites
func entersStateDAG(ev *types.Event) bool {
	return ev.StateKey != nil && !ev.Rejected && !ev.SoftFailed
}

// advanceStateExtremities returns a state DAG room's state extremities once an event publishes,
// the change its publish writes to events/srx
func advanceStateExtremities(extremities []id.EventID, ev *types.Event) []id.EventID {
	if !entersStateDAG(ev) {
		return extremities
	}
	return advanceExtremities(extremities, ev.PrevStateEventIDs, ev.ID)
}

// citedExtremities returns the extremities a local event of a state DAG room cites, of extremities
// in the order they were stored: all of them up to types.MaxStateDAGPrevEvents, otherwise that many
// stored last. Depth would let a sender choose.
func citedExtremities(extremities []id.EventID) []id.EventID {
	return append([]id.EventID{}, extremities[max(len(extremities)-types.MaxStateDAGPrevEvents, 0):]...)
}

// The most state events send_join serves of a state DAG room, which it reads in one transaction
// until it streams them
const maxServedStateDAG = 10_000

var ErrStateDAGTooLarge = fmt.Errorf("the room's state DAG has more than %d events, more than this server serves yet", maxServedStateDAG)

// CheckServedStateDAG fails with ErrStateDAGTooLarge for a state DAG room with more state events than
// GetServedStateDAG serves
func (r *RoomsDatabase) CheckServedStateDAG(ctx context.Context, roomID id.RoomID) error {
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		if _, more, err := r.events.TxnLookupRoomStateDAGEventIDs(txn, roomID, maxServedStateDAG); err != nil {
			return nil, err
		} else if more {
			return nil, ErrStateDAGTooLarge
		}
		return nil, nil
	})
	return err
}

// GetServedStateDAG returns a state DAG room's state DAG as published, every accepted state event in
// the order it was published, failing with ErrStateDAGTooLarge past maxServedStateDAG
func (r *RoomsDatabase) GetServedStateDAG(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventIDs, more, err := r.events.TxnLookupRoomStateDAGEventIDs(txn, roomID, maxServedStateDAG)
		if err != nil {
			return nil, err
		} else if more {
			return nil, ErrStateDAGTooLarge
		}
		return r.events.NewTxnEventsProvider(ctx, txn).GetAll(eventIDs)
	})
}

// Events a get_missing_events walk of a state DAG reads per transaction
const stateDAGWalkChunk = 1000

// GetMissingStateDAGEvents answers get_missing_events in a state DAG room with the MSC's state_dag
// flag, see walkStateDAG
func (r *RoomsDatabase) GetMissingStateDAGEvents(ctx context.Context, roomID id.RoomID, earliest, latest []id.EventID, limit int) ([]*types.Event, error) {
	return walkStateDAG(roomID, earliest, latest, limit, func(eventIDs []id.EventID) (map[id.EventID]*types.Event, error) {
		return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (map[id.EventID]*types.Event, error) {
			eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
			eventsProvider.WillGet(eventIDs...)
			evs := make(map[id.EventID]*types.Event, len(eventIDs))
			for _, eventID := range eventIDs {
				ev, err := eventsProvider.Get(eventID)
				if err != nil {
					return nil, err
				}
				evs[eventID] = ev
			}
			return evs, nil
		})
	})
}

// walkStateDAG walks a state DAG back from latest along prev_state_events, a message's included, as
// far as earliest, and returns at most limit of the events it reaches, those fewer hops from latest
// first, then by event ID in byte order (MSC4242). Neither latest nor earliest are returned. Only
// events with state here that were not rejected are returned and walked through. Each hop's events
// are read in chunks of stateDAGWalkChunk, in the order they are returned.
func walkStateDAG(
	roomID id.RoomID,
	earliest, latest []id.EventID,
	limit int,
	read func([]id.EventID) (map[id.EventID]*types.Event, error),
) ([]*types.Event, error) {
	seen := make(map[id.EventID]struct{}, len(earliest)+len(latest))
	for _, eventID := range earliest {
		seen[eventID] = struct{}{}
	}
	unseen := func(eventIDs []id.EventID) []id.EventID {
		var next []id.EventID
		for _, eventID := range eventIDs {
			if _, found := seen[eventID]; !found {
				seen[eventID] = struct{}{}
				next = append(next, eventID)
			}
		}
		slices.Sort(next)
		return next
	}
	walkable := func(ev *types.Event) bool {
		return ev.HasStateIn(roomID) && !ev.Rejected
	}

	var found []*types.Event
	frontier, err := readWalkable(read, unseen(latest), walkable, -1)
	for err == nil && len(frontier) > 0 && len(found) < limit {
		var cited []id.EventID
		for _, ev := range frontier {
			cited = append(cited, ev.PrevStateEventIDs...)
		}
		frontier, err = readWalkable(read, unseen(cited), walkable, limit-len(found))
		found = append(found, frontier...)
	}
	return found, err
}

// readWalkable reads the events in chunks, keeping their order, and returns those walkable, at
// most limit of them unless it is negative
func readWalkable(
	read func([]id.EventID) (map[id.EventID]*types.Event, error),
	eventIDs []id.EventID,
	walkable func(*types.Event) bool,
	limit int,
) ([]*types.Event, error) {
	var evs []*types.Event
	for chunk := range slices.Chunk(eventIDs, stateDAGWalkChunk) {
		chunkEvs, err := read(chunk)
		if err != nil {
			return nil, err
		}
		for _, eventID := range chunk {
			if ev := chunkEvs[eventID]; walkable(ev) {
				evs = append(evs, ev)
				if len(evs) == limit {
					return evs, nil
				}
			}
		}
	}
	return evs, nil
}
