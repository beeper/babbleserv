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

// A local user's remote join to a state DAG room (MSC4242) runs in the room's ingest job: phase A
// stages the send_join state DAG and checks it reaches the create event, phase B replays it, then
// the join publishes, see NewStateDAGRemoteJoin.

// StageStateDAGEvents stores verified events without state. Their auth headers stay unfinalized,
// citing nothing, until a replay calculates their auth events.
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

// stateDAGWalk is what a walk back along prev_state_events reached of the state DAG not published
// here. Published events end it, as do events of other rooms, which MSC4242 section 2 rejects citing.
type stateDAGWalk struct {
	cites       map[id.EventID][]id.EventID
	unpublished map[id.EventID]struct{}
	missing     map[id.EventID]struct{}
	pending     map[id.EventID]struct{}
	evs         map[id.EventID]*types.Event
}

func walkUnreplayed(
	roomID id.RoomID,
	fromIDs, pendingIDs []id.EventID,
	keepEvents bool,
	read func([]id.EventID) (map[id.EventID]*types.Event, error),
) (*stateDAGWalk, error) {
	walk := &stateDAGWalk{
		cites:       make(map[id.EventID][]id.EventID),
		unpublished: make(map[id.EventID]struct{}),
		missing:     make(map[id.EventID]struct{}),
		pending:     make(map[id.EventID]struct{}),
	}
	if keepEvents {
		walk.evs = make(map[id.EventID]*types.Event)
	}
	pending := make(map[id.EventID]struct{}, len(pendingIDs))
	for _, eventID := range pendingIDs {
		pending[eventID] = struct{}{}
	}
	seen := make(map[id.EventID]struct{})
	var frontier []id.EventID
	visit := func(eventIDs []id.EventID) {
		for _, eventID := range eventIDs {
			if _, found := seen[eventID]; found {
				continue
			}
			seen[eventID] = struct{}{}
			if _, found := pending[eventID]; found {
				walk.pending[eventID] = struct{}{}
			} else {
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
			ev := evs[eventID]
			switch {
			case ev == nil || ev.Outlier:
				walk.missing[eventID] = struct{}{}
				continue
			case ev.RoomID != roomID || ev.HasStateIn(roomID) && !ev.Unpublished:
				continue
			case ev.Unpublished:
				walk.unpublished[eventID] = struct{}{}
			}
			walk.cites[eventID] = ev.AuthDependencyIDs()
			if walk.evs != nil {
				walk.evs[eventID] = ev
			}
			visit(walk.cites[eventID])
		}
	}
	return walk, nil
}

// MissingStateDAGEvents returns the events the join reaches along prev_state_events that are not
// stored: none once the send_join state DAG connects it to the create event, as the MSC requires
func (r *RoomsDatabase) MissingStateDAGEvents(ctx context.Context, joinEv *types.Event) ([]id.EventID, error) {
	read, err := r.renewableEventReads(ctx, joinEv.RoomID)
	if err != nil {
		return nil, err
	}
	walk, err := walkUnreplayed(joinEv.RoomID, joinEv.AuthDependencyIDs(), nil, false, read)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(walk.missing)), nil
}

// replayOrder is Kahn's algorithm, lowest event ID first, leaving out and counting events reaching a
// missing or pending one, or a cycle, which event IDs hashing what they cite cannot form
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
			} else if _, pending := walk.pending[citedID]; pending {
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

// Per replay transaction, well within FoundationDB's five-second transaction limit
const (
	maxReplayBatch = 1000
	replayTxnTime  = 2 * time.Second
)

type StateDAGReplay struct {
	RoomVersion string
	// Replayed with the events they reach along prev_state_events
	From []id.EventID
	// Published by the room's job instead, leaving events citing one without state: a join, or the
	// PDUs of a gap fill
	Pending []id.EventID
	// Keeps the replayed state events Unpublished for the job to publish. Otherwise they are history
	// before a join and enter the room's state DAG, as do the unpublished events the replay reaches.
	Publish bool
}

// ReplayStateDAG gives contexts, in topological order, to the stored events without them that From
// reaches, leaving the room record, extremities and timeline alone. The contexts are the progress,
// so a resumed replay walks again and goes on.
func (r *RoomsDatabase) ReplayStateDAG(ctx context.Context, roomID id.RoomID, replay StateDAGReplay) error {
	log := zerolog.Ctx(ctx).With().Stringer("room_id", roomID).Bool("publish", replay.Publish).Logger()
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return err
	}
	walk, err := walkUnreplayed(roomID, replay.From, replay.Pending, false, read)
	if err != nil {
		return err
	}
	order, unreplayable := replayOrder(walk)
	if unreplayable > 0 {
		log.Warn().Int("events", unreplayable).Msg("State DAG events reach events missing or not published here, leaving them without state")
	}
	start := time.Now()
	sender := r.newEventSender(roomID, pushContext{})
	built := make(builtArtifacts)
	var replayed, rejected, transactions int
	for len(order) > 0 {
		batch, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (*replayBatch, error) {
			return r.txnReplay(ctx, txn, roomID, replay, order, walk)
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
			roomVersion: replay.RoomVersion,
		}
		if err := built.check(work); err != nil {
			return err
		} else if err := sender.buildStateResolution(ctx, work); err != nil {
			return err
		}
		built.add(work)
	}
	if replayed > 0 || transactions > 1 {
		log.Info().
			Int("replayed", replayed).
			Int("rejected", rejected).
			Int("transactions", transactions).
			Dur("duration", time.Since(start)).
			Msg("Replayed state DAG events")
	}
	return nil
}

// replayBatch.done counts events found replayed too; request is a resolution the next one needs
type replayBatch struct {
	done, replayed, rejected int
	request                  *eventsendutil.ResolutionRequest
}

// txnReplay reads events with normal reads and skips those with contexts, so an overlapping runner
// conflicts rather than replaying an event twice
func (r *RoomsDatabase) txnReplay(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	replay StateDAGReplay,
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
	resolver := sender.newResolver(txn, prepared, replay.RoomVersion)
	budget := r.config.Rooms.StateBudget.StagingBytes
	batch := &replayBatch{}
	var adopted []*types.Event
	eventBytes := 0
	full := func() bool {
		return eventBytes+prepared.stateBatch.PendingBytes(prepared.stateContexts()...) >= budget || time.Since(start) > replayTxnTime
	}
	for _, eventID := range order {
		ev, err := eventsProvider.GetRequired(eventID)
		if err != nil {
			return nil, err
		} else if ev.HasStateIn(roomID) {
			batch.done++
			if !ev.Unpublished || replay.Publish {
				continue
			}
			ev.Unpublished = false
			adopted = append(adopted, ev)
			if eventBytes += len(ev.ToMsgpack()); full() {
				break
			}
			continue
		}
		if batch.request, err = r.txnReplayEvent(ctx, txn, prepared, resolver, ev); err != nil {
			return nil, err
		} else if batch.request != nil {
			break
		}
		prepared.evs = append(prepared.evs, ev)
		adopted = append(adopted, ev)
		batch.done++
		if ev.Rejected {
			batch.rejected++
		}
		if eventBytes += len(ev.ToMsgpack()) + stagedEventOverhead; full() {
			break
		}
	}
	batch.replayed = len(prepared.evs)
	for _, ev := range prepared.evs {
		ev.Unpublished = replay.Publish && isAcceptedState(ev)
	}
	r.events.TxnAdoptEvents(txn, adopted...)
	prepared.stateBatch.TxnWrite(txn, prepared.stateContexts()...)
	prepared.authGraph.Write(txn)
	if !replay.Publish {
		for i, ev := range adopted {
			if isAcceptedState(ev) {
				r.events.TxnAddRoomStateDAGEvent(txn, roomID, ev.ID, tuple.IncompleteVersionstamp(uint16(i)))
			}
		}
	}
	return batch, nil
}

// txnReplayEvent gives an event its contexts as step 5 gives a federated one its state, or returns
// the resolution it needs beyond the inline budgets, leaving the event as it was
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
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	// As the pipeline gives a published state event the content it replaces, for sync
	var prevStateEventID id.EventID
	if ev.StateKey != nil {
		prevStateEventID = authState[ev.StateTup()]
	}
	return nil, r.txnPreProcessEventUnsigned(txn, prepared.eventsProvider, ev, prevStateEventID)
}

// stateDAGJoinBefore resolves the states after the join's prev state events, which the job replayed
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

// stateDAGJoinTips returns the served tips that stay state extremities beside the join: its template
// cites at most 20 of the resident's, and state may arrive there between make_join and send_join.
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
