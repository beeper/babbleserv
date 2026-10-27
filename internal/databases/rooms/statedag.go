package rooms

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// A state DAG room's events cite only events with state here
var errStateDAGGivenStates = errors.New("state DAG rooms take no given states")

func isStateDAGSend(room *types.Room, evs []*types.Event) bool {
	return room != nil && util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) ||
		slices.ContainsFunc(evs, (*types.Event).StateDAGs)
}

// In a state DAG room only messages soft fail, as state events always join the state DAG
func softFails(ev *types.Event) bool {
	return !ev.StateDAGs() || ev.StateKey == nil
}

func entersStateDAG(ev *types.Event) bool {
	return ev.StateKey != nil && !ev.Rejected && !ev.SoftFailed
}

// advanceStateExtremities is the change an event's publish writes to events/srx
func advanceStateExtremities(extremities []id.EventID, ev *types.Event) []id.EventID {
	if !entersStateDAG(ev) {
		return extremities
	}
	return advanceExtremities(extremities, ev.PrevStateEventIDs, ev.ID)
}

// citedExtremities cites those stored last up to the cap, as ordering by depth would let a sender choose
func citedExtremities(extremities []id.EventID) []id.EventID {
	return append([]id.EventID{}, extremities[max(len(extremities)-types.MaxStateDAGPrevEvents, 0):]...)
}

// citedPrevStateEvents adds the membership an out-of-band change supersedes, in place of the oldest
// extremity at the cap. current means the state before the event is current state.
func citedPrevStateEvents(extremities []id.EventID, superseded id.EventID) (cited []id.EventID, current bool) {
	cited = citedExtremities(extremities)
	if superseded == "" || slices.Contains(cited, superseded) {
		return cited, len(cited) == len(extremities)
	} else if len(cited) == types.MaxStateDAGPrevEvents {
		cited = cited[1:]
	}
	return append(cited, superseded), false
}

func rescindedMembership(held types.StateEntry) id.EventID {
	switch held.Membership {
	case event.MembershipInvite, event.MembershipKnock:
		return held.EventID
	}
	return ""
}

// changesRemoteMembership reports an out-of-band change, should the target's server not be in the
// room. A make_* template is the target's own event, sent back by its server.
func changesRemoteMembership(ev *types.Event, serverName string) bool {
	if ev.Type != event.StateMember || ev.StateKey == nil {
		return false
	}
	target := id.UserID(*ev.StateKey)
	return target != ev.Sender && target.Homeserver() != serverName
}

var ErrStateDAGTooLarge = errors.New("the room's state DAG has more events than this server serves")

// CheckServedStateDAG counts the join about to publish against Federation.MaxServedStateDAG
func (r *RoomsDatabase) CheckServedStateDAG(ctx context.Context, roomID id.RoomID) error {
	limit := r.config.Federation.MaxServedStateDAG
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		if longer, err := r.events.TxnRoomStateDAGLonger(txn, roomID, limit-1); err != nil {
			return nil, err
		} else if longer {
			return nil, fmt.Errorf("%w: more than %d events", ErrStateDAGTooLarge, limit)
		}
		return nil, nil
	})
	return err
}

const servedStateDAGPage = 500

// ErrJoinNotInStateDAG should not happen, as every join accepted here is in the state DAG
var ErrJoinNotInStateDAG = errors.New("join is not in the room's state DAG")

// StreamServedStateDAG pages the state events published before the join, in publish order. Synapse
// serves only the join's ancestors, and would otherwise store the join itself without state first.
func (r *RoomsDatabase) StreamServedStateDAG(ctx context.Context, roomID id.RoomID, joinID id.EventID, each func([]*types.Event) error) error {
	type page struct {
		evs    []*types.Event
		next   fdb.Key
		atJoin bool
	}
	var after fdb.Key
	for {
		pg, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (page, error) {
			eventIDs, next, err := r.events.TxnRoomStateDAGPage(txn, roomID, after, servedStateDAGPage)
			if err != nil {
				return page{}, err
			}
			i := slices.Index(eventIDs, joinID)
			if i >= 0 {
				eventIDs = eventIDs[:i]
			}
			evs, err := r.events.NewTxnEventsProvider(ctx, txn).GetAll(eventIDs)
			return page{evs, next, i >= 0}, err
		})
		if err != nil {
			return err
		} else if err := each(pg.evs); err != nil {
			return err
		} else if pg.atJoin {
			return nil
		} else if pg.next == nil {
			return fmt.Errorf("%w: %s", ErrJoinNotInStateDAG, joinID)
		}
		after = pg.next
	}
}

const stateDAGWalkChunk = 1000

// GetMissingStateDAGEvents answers get_missing_events with MSC4242's state_dag flag
func (r *RoomsDatabase) GetMissingStateDAGEvents(ctx context.Context, roomID id.RoomID, earliest, latest []id.EventID, limit int) ([]*types.Event, error) {
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return walkStateDAG(roomID, earliest, latest, limit, read)
}

// renewableEventReads shares a transaction across reads until it is too old or fails retryably, so
// a walk reading one hop at a time is not a transaction a hop
func (r *RoomsDatabase) renewableEventReads(ctx context.Context, roomID id.RoomID) (func([]id.EventID) (map[id.EventID]*types.Event, error), error) {
	reads, err := eventsendutil.NewArtifactReads(ctx, r.db, r.events, roomID, nil, nil)
	if err != nil {
		return nil, err
	}
	return func(eventIDs []id.EventID) (map[id.EventID]*types.Event, error) {
		evs := make(map[id.EventID]*types.Event, len(eventIDs))
		return evs, reads.Read(func(txn fdb.ReadTransaction) error {
			eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
			eventsProvider.WillGet(eventIDs...)
			for _, eventID := range eventIDs {
				ev, err := eventsProvider.Get(eventID)
				if err != nil {
					return err
				}
				evs[eventID] = ev
			}
			return nil
		})
	}, nil
}

// walkStateDAG walks back from latest along prev_state_events as far as earliest, excluding both,
// returning at most limit events, fewer hops first then by event ID in byte order (MSC4242). Only
// unrejected events with state here are returned and walked through.
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

// readWalkable keeps the events' order, returning at most limit unless it is negative
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
