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

// citedPrevStateEvents returns the prev_state_events of a local event of a state DAG room: the
// extremities citedExtremities gives, and the membership an out-of-band change supersedes, when
// given and not among them, in place of the extremity stored first should they fill the cap. It
// reports whether they are every extremity and nothing else, when the state before the event is
// current state.
func citedPrevStateEvents(extremities []id.EventID, superseded id.EventID) (cited []id.EventID, current bool) {
	cited = citedExtremities(extremities)
	if superseded == "" || slices.Contains(cited, superseded) {
		return cited, len(cited) == len(extremities)
	} else if len(cited) == types.MaxStateDAGPrevEvents {
		cited = cited[1:]
	}
	return append(cited, superseded), false
}

// rescindedMembership returns the membership a leave of its target rescinds, held in current state
func rescindedMembership(held types.StateEntry) id.EventID {
	switch held.Membership {
	case event.MembershipInvite, event.MembershipKnock:
		return held.EventID
	}
	return ""
}

// changesRemoteMembership reports whether a member event changes the membership of a user of a
// server other than serverName, sent by someone else: an out-of-band change, should the target's
// server not be in the room. A make_* template is the target's own event, sent back by its server.
func changesRemoteMembership(ev *types.Event, serverName string) bool {
	if ev.Type != event.StateMember || ev.StateKey == nil {
		return false
	}
	target := id.UserID(*ev.StateKey)
	return target != ev.Sender && target.Homeserver() != serverName
}

// ErrStateDAGTooLarge refuses a join to a state DAG room whose state DAG, with the join, would hold
// more events than this server serves
var ErrStateDAGTooLarge = errors.New("the room's state DAG has more events than this server serves")

// CheckServedStateDAG fails with ErrStateDAGTooLarge for a state DAG room whose state DAG would hold
// more than config.Federation.MaxServedStateDAG events with one more, as a join's publish adds
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

// Events of a state DAG that send_join serves per transaction
const servedStateDAGPage = 500

// ErrJoinNotInStateDAG fails serving a state DAG for a join that is not in it, which every join
// accepted here is
var ErrJoinNotInStateDAG = errors.New("join is not in the room's state DAG")

// StreamServedStateDAG hands each the part of a state DAG room's state DAG served with a join: every
// accepted state event published before the join, in the order it was published, a page per
// transaction. Neither the join nor what was published after it are served, as Synapse serves only
// the join's ancestors, and would otherwise store the join itself without state first.
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

// Events a walk of a state DAG reads per batch of reads, which share a transaction until it is too
// old
const stateDAGWalkChunk = 1000

// GetMissingStateDAGEvents answers get_missing_events in a state DAG room with the MSC's state_dag
// flag, see walkStateDAG
func (r *RoomsDatabase) GetMissingStateDAGEvents(ctx context.Context, roomID id.RoomID, earliest, latest []id.EventID, limit int) ([]*types.Event, error) {
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return walkStateDAG(roomID, earliest, latest, limit, read)
}

// renewableEventReads returns a reader of events that reads each batch in the transaction the last
// one used, starting another once it is too old or failed with a retryable error, as one too old
// does, so that a walk reading one hop of a state DAG at a time is not a transaction a hop
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
