package rooms

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var (
	errServerJoined    = errors.New("this server joined the room meanwhile")
	errJoinStored      = errors.New("the join is stored already")
	errJoinUnpublished = errors.New("the join is stored without state and this server is not joined yet")
)

// remoteJoin is a local user's join to a room with the state before it given, as the resident
// server's send_join response gives it
type remoteJoin struct {
	ev       *types.Event
	stateIDs []id.EventID
	// The room the response gives, which the join's publish stores when it has no record
	room *types.Room
	// The state before the join, the response state less the events failing their own auth events
	state types.StateEntries
	// Response events stored rejected that the join accepts, which its publish stores accepted
	readopt []id.EventID
	// The state before the join once staged, and the changes to it from the room's current state
	boundary types.StateHash
	diff     *boundaryChanges
	// Another join got this server into the room, so the join is sent as any federated event
	asMember bool
	// Stored already, so the send reports the stored outcome, see txnStoredJoinResult
	stored bool
}

// boundaryChanges are the changes from the room's current state, from, to the state before a remote
// join, diffed ahead of the join's publish, which then diffs only the join's own step
type boundaryChanges struct {
	from, state types.StateHash
	changes     []types.StateChange
}

// remoteJoinIn returns the join of a batch that is a local user's join with a given before-state,
// nil for any other batch
func (r *RoomsDatabase) remoteJoinIn(evs []*types.Event, given *GivenStates) *remoteJoin {
	if given == nil || len(evs) != 1 || evs[0].StateKey == nil || !isJoinEvent(evs[0]) {
		return nil
	} else if stateIDs, found := given.BeforeEvents[evs[0].ID]; found && r.isLocalUser(id.UserID(*evs[0].StateKey)) {
		return &remoteJoin{ev: evs[0], stateIDs: stateIDs}
	}
	return nil
}

// inJoinMode reports whether a send publishes a remote join that replaces the room's state
func (join *remoteJoin) inJoinMode() bool {
	return join != nil && !join.asMember
}

// readopted returns the response events the join's publish stores accepted: none once the join is
// sent as a member before its state was built, as the state built then leaves them out.
func (join *remoteJoin) readopted() []id.EventID {
	if join == nil || (join.asMember && join.diff == nil) {
		return nil
	}
	return join.readopt
}

// remoteJoinSender prepares a local user's join to a room this server is not in: it checks the
// join and stages the response events, a job builds the state before the join, then the join
// publishes.
type remoteJoinSender struct {
	*federatedEventSender
	// The events the last check kept to stage, checked again unless staged
	staging *stagingEvents
}

func (s *remoteJoinSender) send(ctx context.Context) (*SendEventsResult, error) {
	res, err := s.sendInJoinTurn(ctx)
	if errors.Is(err, errJoinStored) {
		s.join.stored = true
		return s.run(ctx, s.prepare)
	}
	return res, err
}

func (s *remoteJoinSender) sendInJoinTurn(ctx context.Context) (*SendEventsResult, error) {
	unlock, err := s.r.lockJoin(ctx, s.roomID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.run(ctx, s.prepare)
}

func (s *remoteJoinSender) prepare(ctx context.Context) (preparationOutcome, error) {
	if s.join.stored {
		return s.storedJoinResult(ctx)
	} else if !s.join.asMember {
		outcome, err := s.prepareJoin(ctx)
		if !errors.Is(err, errServerJoined) {
			return outcome, err
		}
		zerolog.Ctx(ctx).Info().Stringer("event_id", s.join.ev.ID).Msg("Another join got this server into the room, sending the join as a member")
		s.join.asMember = true
		if !s.join.boundary.IsZero() {
			s.given.boundaries = map[id.EventID]types.StateHash{s.join.ev.ID: s.join.boundary}
		}
	}
	return s.federatedEventSender.prepare(ctx)
}

func (p *remoteJoinSender) prepareJoin(ctx context.Context) (preparationOutcome, error) {
	if p.staging == nil || !p.staging.staged {
		staging, err := p.checkRemoteJoin(ctx)
		if err != nil {
			return nil, err
		}
		p.staging = staging
		return needsWork{work: stageEventsWork{staging: staging}}, nil
	}
	outcome, err := util.DoReadTransaction(ctx, p.r.db, func(txn fdb.ReadTransaction) (preparationOutcome, error) {
		return p.txnPrepareFederated(ctx, txn, p.attemptEvents(p.evs), nil)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to prepare the join: %w", err)
	}
	return outcome, nil
}

// checkRemoteJoin checks the given events (from the other server), and the join event against their
// own auth events. Returns the given events to stage and updates the join event.
func (s *remoteJoinSender) checkRemoteJoin(ctx context.Context) (*stagingEvents, error) {
	join := s.join
	inState := make(map[id.EventID]struct{}, len(join.stateIDs))
	for _, eventID := range join.stateIDs {
		inState[eventID] = struct{}{}
	}
	room, err := util.DoReadTransaction(ctx, s.r.db, func(txn fdb.ReadTransaction) (*types.Room, error) {
		return s.txnCheckRemoteJoinRoom(txn, inState)
	})
	if err != nil {
		return nil, err
	}

	reads, err := eventsendutil.NewArtifactReads(ctx, s.r.db, s.r.events, s.roomID, nil, nil)
	if err != nil {
		return nil, err
	}
	joinEv := s.attemptEvents([]*types.Event{join.ev})[0]
	kept, rejectedAuth, err := s.checkRemoteJoinAuth(ctx, reads, joinEv, s.attemptEvents(s.fetched))
	if err != nil {
		return nil, err
	}

	// A state response names one event for each tuple, the join fails on any other
	stateEvs := slices.DeleteFunc(slices.Clone(kept), func(ev *types.Event) bool {
		_, found := inState[ev.ID]
		return !found
	})
	joinState, err := s.stateFromEvents(stateEvs)
	if err != nil {
		return nil, fmt.Errorf("invalid state before the join: %w", err)
	}
	if err := reads.Do(func() error {
		authErr, err := events.NewTxnAuthEventsProvider(ctx, reads.Events(), joinState.EventIDs()).IsEventAllowed(joinEv)
		if err != nil {
			return err
		} else if authErr != nil {
			return fmt.Errorf("%w: %w", ErrAuthStage5, authErr)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	staged, readopt, err := s.eventsToStage(ctx, kept, true)
	if err != nil {
		return nil, err
	}
	join.room, join.state, join.readopt = room, joinState, slices.Sorted(maps.Keys(readopt))
	join.boundary, join.diff = types.StateHash{}, nil
	return &stagingEvents{evs: staged, readopt: readopt, rejectedAuth: rejectedAuth}, nil
}

// txnCheckRemoteJoinRoom checks the room a join is to, and that the join is not stored, and gives
// the room the join's publish stores when it has no record: from the create event of the state
// before the join.
func (s *remoteJoinSender) txnCheckRemoteJoinRoom(
	txn fdb.ReadTransaction,
	inState map[id.EventID]struct{},
) (*types.Room, error) {
	joinEv := s.join.ev
	// Each event is staged under the room it names
	for _, ev := range slices.Concat(s.fetched, []*types.Event{joinEv}) {
		if ev.RoomID != s.roomID {
			return nil, fmt.Errorf("event %s in the join to %s is for room %s", ev.ID, s.roomID, ev.RoomID)
		}
	}

	joinVersionFut := txn.Get(s.r.events.KeyForIDToVersion(joinEv.ID))
	room, err := s.r.txnGetRoom(txn, s.roomID)
	if errors.Is(err, types.ErrRoomNotFound) {
		roomCreate := []*types.PartialEvent{&joinEv.PartialEvent}
		for _, ev := range s.fetched {
			if _, found := inState[ev.ID]; found && ev.Type == event.StateCreate && ev.StateKey != nil && *ev.StateKey == "" {
				roomCreate = []*types.PartialEvent{&ev.PartialEvent}
				break
			}
		}
		room, err = s.r.newRoomFromEvents(s.roomID, roomCreate)
	} else if err == nil && isServerJoined(room) {
		return nil, errServerJoined
	}
	if err != nil {
		return nil, err
	} else if joinVersionFut.MustGet() != nil {
		return nil, errJoinStored
	} else if !room.Federated {
		return nil, errRoomNotFederated
	}
	// A create event redacted before room version 11 has lost its room version, the join carries the
	// version the resident server gave for the room
	room.Version = cmp.Or(room.Version, joinEv.RoomVersion)
	return room, nil
}

// storedJoinResult answers a remote join stored already, see txnStoredJoinResult
func (s *remoteJoinSender) storedJoinResult(ctx context.Context) (preparationOutcome, error) {
	res, err := util.DoReadTransaction(ctx, s.r.db, func(txn fdb.ReadTransaction) (*SendEventsResult, error) {
		return s.txnStoredJoinResult(txn)
	})
	if err != nil {
		return nil, err
	}
	return alreadySent{result: res}, nil
}

// checkRemoteJoinAuth checks the join and the response events against their own auth events, and
// returns the response events to keep, in topological order, and the stored events cited that it
// found rejected.
func (s *remoteJoinSender) checkRemoteJoinAuth(
	ctx context.Context,
	reads *eventsendutil.ArtifactReads,
	joinEv *types.Event,
	responseEvs []*types.Event,
) ([]*types.Event, map[id.EventID]struct{}, error) {
	// The join goes first so it is the copy checked when the response repeats it
	evs := s.sortEventsTopologically(slices.Concat([]*types.Event{joinEv}, responseEvs))
	dropped, rejectedAuth, err := s.checkOwnAuthEvents(ctx, reads, evs, joinEv)
	if err != nil {
		return nil, nil, err
	}
	return slices.DeleteFunc(evs, func(ev *types.Event) bool {
		_, found := dropped[ev.ID]
		return found || ev.ID == joinEv.ID
	}), rejectedAuth, nil
}

// stateFromEvents is the state another server gave as events, without those that are unknown or not
// accepted state of the room. Two events for one tuple are inconsistent.
func (s *remoteJoinSender) stateFromEvents(evs []*types.Event) (types.StateEntries, error) {
	stateMap := make(types.StateEntries, len(evs))
	for _, ev := range evs {
		if !s.isRoomState(ev) {
			continue
		} else if err := s.addGivenState(stateMap, ev.StateTup(), ev.StateEntry()); err != nil {
			return nil, err
		}
	}
	return stateMap, nil
}
