package rooms

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// A local user's join to a state DAG room (MSC4242) another server hosts runs in the room's ingest
// job: phase A stages the state DAG the resident's send_join gives (StageStateDAGEvents) and checks
// it connects the join to the create event (MissingStateDAGEvents), phase B replays it here
// (ReplayStateDAG), then the join publishes as a RemoteJoin, see NewStateDAGRemoteJoin.

// StageStateDAGEvents stores verified events of a state DAG room without state, other than those
// stored already, in chunks of the staging budget. Their auth events are calculated, and their auth
// headers finalized, once they are replayed: until then their headers are unfinalized and cite
// nothing.
func (r *RoomsDatabase) StageStateDAGEvents(ctx context.Context, roomID id.RoomID, evs []*types.Event) error {
	for _, ev := range evs {
		if ev.RoomID != roomID {
			return fmt.Errorf("event %s staged to %s is for room %s", ev.ID, roomID, ev.RoomID)
		}
		r.stripEventUnsigned(ev)
	}
	sender := &federatedEventSender{eventSender: r.newEventSender(roomID, pushContext{})}
	for _, chunk := range sender.chunkStatelessEvents(evs) {
		if _, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
			stored, err := r.txnGetStoredEventIDs(txn, util.EventsToIDs(chunk))
			if err != nil {
				return nil, err
			}
			r.events.TxnStoreEventRows(txn, false, slices.DeleteFunc(slices.Clone(chunk), func(ev *types.Event) bool {
				_, found := stored[ev.ID]
				return found
			})...)
			return nil, nil
		}); err != nil {
			return err
		}
		util.ReportProgress(ctx)
	}
	return nil
}

// stateDAGWalk is what a walk back along prev_state_events reached of a state DAG room's state DAG
// without contexts here: the events stored without, each with the events it cites, the create among
// them, and the events missing, not stored or stored only as outliers. Events with contexts end the
// walk, as do events of other rooms, which the MSC's section 2 rejects citing.
type stateDAGWalk struct {
	cites   map[id.EventID][]id.EventID
	missing map[id.EventID]struct{}
}

func walkUnreplayed(
	roomID id.RoomID,
	fromIDs []id.EventID,
	read func([]id.EventID) (map[id.EventID]*types.Event, error),
) (*stateDAGWalk, error) {
	walk := &stateDAGWalk{cites: make(map[id.EventID][]id.EventID), missing: make(map[id.EventID]struct{})}
	seen := make(map[id.EventID]struct{})
	var frontier []id.EventID
	visit := func(eventIDs []id.EventID) {
		for _, eventID := range eventIDs {
			if _, found := seen[eventID]; !found {
				seen[eventID] = struct{}{}
				frontier = append(frontier, eventID)
			}
		}
	}
	visit(fromIDs)
	for len(frontier) > 0 {
		chunk := slices.Clone(frontier[:min(len(frontier), stateDAGWalkChunk)])
		frontier = frontier[len(chunk):]
		evs, err := read(chunk)
		if err != nil {
			return nil, err
		}
		for _, eventID := range chunk {
			switch ev := evs[eventID]; {
			case ev == nil || ev.Outlier:
				walk.missing[eventID] = struct{}{}
			case ev.RoomID == roomID && !ev.HasStateIn(roomID):
				walk.cites[eventID] = ev.AuthDependencyIDs()
				visit(walk.cites[eventID])
			}
		}
	}
	return walk, nil
}

// MissingStateDAGEvents returns the events that a join to a state DAG room reaches along
// prev_state_events, through events without contexts here, that are not stored: none once the state
// DAG connects the join to the create event, as the MSC requires of the state DAG a send_join gives
func (r *RoomsDatabase) MissingStateDAGEvents(ctx context.Context, joinEv *types.Event) ([]id.EventID, error) {
	read, err := r.renewableEventReads(ctx, joinEv.RoomID)
	if err != nil {
		return nil, err
	}
	walk, err := walkUnreplayed(joinEv.RoomID, joinEv.AuthDependencyIDs(), read)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(walk.missing)), nil
}

// replayOrder orders the events of a walk so each follows the events it cites among them, the lowest
// event ID first of those ready (Kahn's algorithm). An event reaching one missing is left out, and
// counted, as is one on a cycle, which events whose IDs hash what they cite cannot form.
func replayOrder(walk *stateDAGWalk) (order []id.EventID, unreplayable int) {
	eventIDs := slices.Sorted(maps.Keys(walk.cites))
	index := make(map[id.EventID]int, len(eventIDs))
	for i, eventID := range eventIDs {
		index[eventID] = i
	}
	citedBy := make([][]int, len(eventIDs))
	remaining := make([]int, len(eventIDs))
	for i, eventID := range eventIDs {
		for _, citedID := range slices.Compact(slices.Sorted(slices.Values(walk.cites[eventID]))) {
			if j, found := index[citedID]; found && j != i {
				citedBy[j] = append(citedBy[j], i)
				remaining[i]++
			} else if _, missing := walk.missing[citedID]; missing {
				remaining[i]++
			}
		}
	}
	ready := &indexHeap{}
	for i := range eventIDs {
		if remaining[i] == 0 {
			*ready = append(*ready, i)
		}
	}
	heap.Init(ready)
	order = make([]id.EventID, 0, len(eventIDs))
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		order = append(order, eventIDs[i])
		for _, j := range citedBy[i] {
			remaining[j]--
			if remaining[j] == 0 {
				heap.Push(ready, j)
			}
		}
	}
	return order, len(eventIDs) - len(order)
}

// Most events a replay transaction gives contexts, and how long it goes on giving them, well within
// FoundationDB's five seconds
const (
	maxReplayBatch = 1000
	replayTxnTime  = 2 * time.Second
)

// ReplayStateDAG gives contexts to the events of a state DAG room stored without that a join and the
// tips of the state DAG served with it reach along prev_state_events (phase B of a state DAG join):
// in topological order, each as step 5 gives a federated event its state, see txnReplayEvent, in
// staging transactions. The events stay unpublished, as nothing touches the room record, its
// extremities or its timeline, but its accepted state events join the room's state DAG that
// send_join serves, as the resident server published them. A resolution beyond the inline budgets
// runs as an artifact job between transactions. The contexts are the progress: a replay resumed
// walks again and goes on. Events reaching one missing are left without, so a join citing one finds
// its state unavailable, and a tip citing one stays no state extremity.
func (r *RoomsDatabase) ReplayStateDAG(ctx context.Context, joinEv *types.Event, tips []id.EventID) error {
	roomID := joinEv.RoomID
	log := zerolog.Ctx(ctx).With().Stringer("room_id", roomID).Stringer("event_id", joinEv.ID).Logger()
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return err
	}
	walk, err := walkUnreplayed(roomID, slices.Concat(joinEv.AuthDependencyIDs(), tips), read)
	if err != nil {
		return err
	}
	// The join is published, never replayed, should it be stored without state
	if _, found := walk.cites[joinEv.ID]; found {
		delete(walk.cites, joinEv.ID)
		walk.missing[joinEv.ID] = struct{}{}
	}
	order, unreplayable := replayOrder(walk)
	if unreplayable > 0 {
		log.Warn().Int("events", unreplayable).Msg("State DAG events reach events missing here, leaving them without state")
	}
	start := time.Now()
	sender := r.newEventSender(roomID, pushContext{})
	built := make(builtArtifacts)
	var replayed, rejected, transactions int
	for len(order) > 0 {
		batch, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (*replayBatch, error) {
			return r.txnReplay(ctx, txn, roomID, joinEv.RoomVersion, order, walk)
		})
		if err != nil {
			return fmt.Errorf("failed to replay the state DAG: %w", err)
		}
		order = order[batch.done:]
		replayed, rejected, transactions = replayed+batch.replayed, rejected+batch.rejected, transactions+1
		util.ReportProgress(ctx)
		if batch.request == nil {
			continue
		}
		work := resolveStateWork{
			request:     batch.request,
			inputs:      artifactInputs{stateBatch: r.state.NewBatch(roomID)},
			roomVersion: joinEv.RoomVersion,
		}
		if err := built.check(work); err != nil {
			return err
		} else if err := sender.buildStateResolution(ctx, work); err != nil {
			return err
		}
		built.add(work)
	}
	log.Info().
		Int("replayed", replayed).
		Int("rejected", rejected).
		Int("transactions", transactions).
		Dur("duration", time.Since(start)).
		Msg("Replayed the state DAG of a join")
	return nil
}

// replayBatch is what a replay transaction did: the events of the order it went through, replayed
// by it or found replayed, and a resolution the next event needs first
type replayBatch struct {
	done, replayed, rejected int
	request                  *eventsendutil.ResolutionRequest
}

// txnReplay replays events in order until the transaction holds the staging budget, ends a resolution
// the next event needs beyond the inline budgets, or has run replayTxnTime. It reads the events with
// normal reads and skips those with contexts, so a runner overlapping it conflicts with it rather
// than replaying an event twice. Accepted state events join the room's state DAG in the order given.
func (r *RoomsDatabase) txnReplay(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	roomVersion string,
	order []id.EventID,
	walk *stateDAGWalk,
) (*replayBatch, error) {
	start := time.Now()
	order = order[:min(len(order), maxReplayBatch)]
	eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
	defer eventsProvider.CancelUnread()
	eventsProvider.WillGet(order...)
	for _, eventID := range order {
		eventsProvider.WillGet(walk.cites[eventID]...)
	}
	prepared := &preparedEvents{
		stateBatch:     r.state.NewBatch(roomID),
		authGraph:      r.events.NewAuthGraph(txn, roomID, eventsProvider),
		eventsProvider: eventsProvider,
	}
	sender := r.newEventSender(roomID, pushContext{})
	resolver := sender.newResolver(txn, prepared, roomVersion)
	budget := r.config.Rooms.StateBudget.StagingBytes
	batch := &replayBatch{}
	eventBytes := 0
	for _, eventID := range order {
		ev, err := eventsProvider.GetRequired(eventID)
		if err != nil {
			return nil, err
		} else if ev.HasStateIn(roomID) {
			batch.done++
			continue
		}
		if batch.request, err = r.txnReplayEvent(ctx, txn, prepared, resolver, ev); err != nil {
			return nil, err
		} else if batch.request != nil {
			break
		}
		prepared.evs = append(prepared.evs, ev)
		batch.done++
		if ev.Rejected {
			batch.rejected++
		}
		eventBytes += len(ev.ToMsgpack()) + stagedEventOverhead
		if eventBytes+prepared.stateBatch.PendingBytes(prepared.stateContexts()...) >= budget || time.Since(start) > replayTxnTime {
			break
		}
	}
	batch.replayed = len(prepared.evs)
	r.events.TxnAdoptEvents(txn, prepared.evs...)
	prepared.stateBatch.TxnWrite(txn, prepared.stateContexts()...)
	prepared.authGraph.Write(txn)
	for i, ev := range prepared.evs {
		if isAcceptedState(ev) {
			r.events.TxnAddRoomStateDAGEvent(txn, roomID, ev.ID, tuple.IncompleteVersionstamp(uint16(i)))
		}
	}
	return batch, nil
}

// txnReplayEvent gives a stored event of a state DAG room its contexts as step 5 gives a federated
// one its state: the MSC's section 2, then the state after its prev state events, its calculated auth
// events, and authorization against that state. An event section 2 rejects takes known empty state,
// as the pipeline gives it. Returns a request for a resolution beyond the inline budgets instead,
// leaving the event as it was.
func (r *RoomsDatabase) txnReplayEvent(
	ctx context.Context,
	txn fdb.ReadTransaction,
	prepared *preparedEvents,
	resolver *eventsendutil.Resolver,
	ev *types.Event,
) (*eventsendutil.ResolutionRequest, error) {
	log := zerolog.Ctx(ctx).With().Stringer("event_id", ev.ID).Stringer("type", ev.Type).Logger()
	reject := func(err error) {
		log.Warn().Err(err).Msg("Rejecting replayed state DAG event")
		ev.Rejected = true
	}
	if authErr, err := util.AuthorizePrevStateEvents(ev, prepared.eventsProvider.Get); err != nil {
		return nil, err
	} else if authErr != nil {
		ev.BeforeState, ev.AfterState = state.EmptyContext, state.EmptyContext
		reject(authErr)
		return nil, nil
	}
	known, unknown, err := resolver.PartitionPrevs(ev.PrevStateEventIDs)
	if err != nil {
		return nil, err
	} else if len(unknown) > 0 {
		return nil, fmt.Errorf("%w: %s cites %s", ErrPrevStateEventsUnavailable, ev.ID, unknown)
	}
	before, request, err := resolver.StateBefore(known)
	if err != nil || request != nil {
		return request, err
	}
	ev.BeforeState, ev.AfterState = before, before
	if err := r.txnCheckEventBeforeStore(txn, ev.RoomID, ev); err != nil {
		reject(err)
		return nil, nil
	}
	authState, err := prepared.txnLookupAuthState(txn, before, ev)
	if err != nil {
		return nil, err
	}
	ev.AuthEventIDs = events.AuthEventIDsFor(ev, authState)
	prepared.authGraph.AddCalculated(ev)
	if authErr, err := prepared.authorize(ctx, authState, ev); err != nil {
		return nil, err
	} else if authErr != nil {
		reject(authErr)
		return nil, nil
	}
	if err := prepared.txnApplyEvent(txn, before, ev); errors.Is(err, state.ErrEntryTooLarge) {
		reject(err)
	} else if err != nil {
		return nil, err
	}
	return nil, nil
}

// stateDAGJoinBefore is the state before a join to a state DAG room: the state after its prev state
// events, which the room's job replayed, resolved when there are several, the resolution staged and
// stored for the join's publish
func (s *federatedEventSender) stateDAGJoinBefore(
	ctx context.Context,
	reads *eventsendutil.ArtifactReads,
	stateBatch *state.Batch,
	roomVersion string,
) (types.StateHash, error) {
	var contexts []types.StateHash
	if err := reads.Do(func() error {
		prevs, err := reads.Events().GetAll(s.join.ev.PrevStateEventIDs)
		if err != nil {
			return err
		}
		contexts = contexts[:0]
		for _, prev := range prevs {
			if !prev.HasStateIn(s.roomID) {
				return fmt.Errorf("%w: %s", ErrPrevStateEventsUnavailable, prev.ID)
			} else if !slices.Contains(contexts, prev.AfterState) {
				contexts = append(contexts, prev.AfterState)
			}
		}
		return nil
	}); err != nil {
		return types.StateHash{}, err
	}
	switch len(contexts) {
	case 0:
		return state.EmptyContext, nil
	case 1:
		return contexts[0], nil
	}
	if result, found, err := stateBatch.TxnResolved(nil, contexts)(); err != nil || found {
		return result, err
	}
	resolver := reads.NewResolver(eventsendutil.ResolverOptions{StateBatch: stateBatch, RoomID: s.roomID, RoomVersion: roomVersion})
	return s.buildAndStoreResolution(ctx, resolver, stateBatch, contexts)
}

// stateDAGJoinTips returns those of the tips of the state DAG a resident served with a join that stay
// state extremities beside it: accepted state events with state here that the join does not cite.
// The join's template cites at most 20 of the resident's state extremities, and state may arrive
// there between make_join and send_join, so the join alone would leave out state the resident has.
func (s *federatedEventSender) stateDAGJoinTips(eventsProvider *events.TxnEventsProvider) ([]id.EventID, error) {
	join := s.join.ev
	eventsProvider.WillGet(s.join.tips...)
	var tips []id.EventID
	for _, tipID := range s.join.tips {
		if tipID == join.ID || slices.Contains(join.PrevStateEventIDs, tipID) {
			continue
		} else if tip, err := eventsProvider.Get(tipID); err != nil {
			return nil, err
		} else if tip.HasStateIn(s.roomID) && isAcceptedState(tip) {
			tips = append(tips, tipID)
		}
	}
	slices.Sort(tips)
	return tips, nil
}
