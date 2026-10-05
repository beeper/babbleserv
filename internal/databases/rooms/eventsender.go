package rooms

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var (
	errNeedsStaging             = errors.New("the send needs staging")
	errGuardFailed              = errors.New("the room changed since the send was prepared")
	errEventsStoredConcurrently = fmt.Errorf("%w: events were stored concurrently", errGuardFailed)
)

// Event sending timeouts & retries
const (
	firstSendRetryPause   = 10 * time.Millisecond
	longestSendRetryPause = 250 * time.Millisecond
	sendTimeout           = 60 * time.Second
	remoteJoinTimeout     = 10 * time.Minute
)

// sendGuard is room data checked at publish time to ensure no other process has mutated the room
type sendGuard struct {
	revision uint64
	joined   bool
	unstored []id.EventID
}

// publishPlan contains everything needed to complete (publish) an event send
type publishPlan struct {
	room  *types.Room
	guard sendGuard
	preparedEvents
	// Stored rejected events a remote join accepts, which its publish then un-rejects
	readopted []*types.Event
	// The remote join whose publish replaces every extremity
	resetExtremities id.EventID
	// What else the publish writes: an alias, publication, a transaction ID or a lock refresh
	extra func(fdb.Transaction, *types.Room) error
}

// preparationOutcome is exactly one next action of a send attempt
type preparationOutcome interface{ preparationOutcome() }

type readyToPublish struct{ plan *publishPlan }
type needsWork struct{ work preparationWork }
type alreadySent struct{ result *SendEventsResult }

func (readyToPublish) preparationOutcome() {}
func (needsWork) preparationOutcome()      {}
func (alreadySent) preparationOutcome()    {}

type prepareEvents func(context.Context) (preparationOutcome, error)

// pushContext is what evaluating push rules for a send's events needs of the room's local members.
type pushContext struct {
	rules types.UserPushRulesMap
	rooms types.UserRoomContextMap
}

// eventSender owns a send's shared context and attempt loop. Concrete senders embed it and
// pass their prepare method explicitly to run. Operations are bound once, and replaceable in tests.
type eventSender struct {
	r       *RoomsDatabase
	roomID  id.RoomID
	push    pushContext
	lock    func() (unlock func())
	publish func(context.Context, *publishPlan) (*SendEventsResult, error)
	build   func(context.Context, preparationWork) error
	after   func(context.Context, *SendEventsResult)
	pause   func(context.Context, int) error
}

func (r *RoomsDatabase) newEventSender(roomID id.RoomID, push pushContext) eventSender {
	s := eventSender{
		r:      r,
		roomID: roomID,
		push:   push,
		lock:   func() func() { return r.lockRoom(roomID) },
	}
	s.pause = s.pauseBeforeRetry
	s.publish = s.stageAndPublish
	s.build = s.executePreparationWork
	s.after = s.afterSend
	return s
}

// run prepares and publishes under the room mutex. Preparatory work, notifications and retry
// pauses run after releasing it. The concrete sender survives every attempt; each plan does not.
func (s *eventSender) run(ctx context.Context, prepare prepareEvents) (*SendEventsResult, error) {
	built := make(builtArtifacts)
	for failures := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, work, err := func() (*SendEventsResult, preparationWork, error) {
			defer s.lock()()
			return s.attempt(ctx, prepare)
		}()
		if err == nil && work != nil {
			if err = built.check(work); err == nil {
				err = s.build(ctx, work)
			}
			if err == nil {
				built.add(work)
				continue
			}
		}
		if !errors.Is(err, errGuardFailed) && !errors.Is(err, errJoinUnpublished) {
			if err != nil {
				return nil, err
			}
			s.after(ctx, res)
			return res, nil
		}
		failures++
		level := zerolog.WarnLevel
		if errors.Is(err, errJoinUnpublished) {
			level = zerolog.DebugLevel
		}
		zerolog.Ctx(ctx).WithLevel(level).Err(err).Int("attempt", failures).Msg("Room changed while sending events, starting again")
		if pauseErr := s.pause(ctx, failures); pauseErr != nil {
			return nil, fmt.Errorf("gave up after %d attempts: %w: %w", failures, err, pauseErr)
		}
	}
}

func (s *eventSender) attempt(ctx context.Context, prepare prepareEvents) (*SendEventsResult, preparationWork, error) {
	outcome, err := prepare(ctx)
	if err != nil {
		return nil, nil, err
	}
	switch outcome := outcome.(type) {
	case readyToPublish:
		if outcome.plan != nil {
			res, err := s.publish(ctx, outcome.plan)
			return res, nil, err
		}
	case needsWork:
		if outcome.work != nil {
			return nil, outcome.work, nil
		}
	case alreadySent:
		if outcome.result != nil {
			return outcome.result, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("invalid preparation outcome: %T", outcome)
}

type builtArtifacts map[string]struct{}

func (b builtArtifacts) check(work preparationWork) error {
	key := work.artifactKey()
	if _, again := b[key]; key != "" && again {
		return fmt.Errorf("artifact still needed after its job ran: %s", work)
	}
	return nil
}

func (b builtArtifacts) add(work preparationWork) {
	if key := work.artifactKey(); key != "" {
		b[key] = struct{}{}
	}
}

func (s *eventSender) pauseBeforeRetry(ctx context.Context, failures int) error {
	pause := min(firstSendRetryPause<<min(failures-1, 8), longestSendRetryPause)
	select {
	case <-time.After(pause/2 + rand.N(pause/2)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// withSendTimeout bounds a send whose context has no deadline
func (s *eventSender) withSendTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, found := ctx.Deadline(); found {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// attemptEvents copies events for one attempt to decide on, without the outcome of any other, so an
// attempt never changes the events it was given.
func (s *eventSender) attemptEvents(evs []*types.Event) []*types.Event {
	copies := make([]*types.Event, len(evs))
	for i, ev := range evs {
		copied := *ev
		copied.IsDuplicate, copied.Outlier = false, false
		copied.ResetOutcome()
		copied.Unsigned = maps.Clone(ev.Unsigned)
		copies[i] = &copied
	}
	return copies
}

// Preparation

// stateLookupKeys are the tuples an event is authorized against, plus its own tuple so the lookup
// also yields the state event it replaces.
func stateLookupKeys(ev *types.Event) []types.StateTup {
	keys := events.AuthStateTupsForEvent(ev)
	if ev.StateKey != nil && !slices.Contains(keys, ev.StateTup()) {
		keys = append(keys, ev.StateTup())
	}
	return keys
}

func isAcceptedState(ev *types.Event) bool {
	return ev.StateKey != nil && !ev.Rejected
}

// preparedEvents holds the events, state contexts and current-state steps a publish stores
// together.
type preparedEvents struct {
	stateBatch     *state.Batch
	authGraph      *events.AuthGraph
	eventsProvider *events.TxnEventsProvider
	// Events to store in batch order, without those dropped
	evs      []*types.Event
	rejected []RejectedEvent
	// The extremities each accepted event replaces
	replacedExtremities map[id.EventID][]id.EventID
	stateSteps          []stateStep
	// A remote join's state before it, with the changes to it from the room's current state
	boundary *boundaryChanges
}

// stateContexts are the contexts the publish of prepared events writes: the events' before and after
// contexts, and the room's current state after each step, which a resolution may have made neither.
func (p *preparedEvents) stateContexts() []types.StateHash {
	contexts := make([]types.StateHash, 0, 2*len(p.evs)+len(p.stateSteps))
	for _, ev := range p.evs {
		if ev.IsDuplicate {
			continue
		}
		for _, stateCtx := range []types.StateHash{ev.BeforeState, ev.AfterState} {
			if !stateCtx.IsZero() {
				contexts = append(contexts, stateCtx)
			}
		}
	}
	for _, step := range p.stateSteps {
		contexts = append(contexts, step.state)
	}
	return contexts
}

// txnLookupAuthState looks up the state an event is authorized against in a context and starts
// fetching its events.
func (p *preparedEvents) txnLookupAuthState(
	txn fdb.ReadTransaction,
	at types.StateHash,
	ev *types.Event,
) (types.StateMap, error) {
	entries, err := p.stateBatch.TxnLookupEntries(txn, at, stateLookupKeys(ev))
	if err != nil {
		return nil, fmt.Errorf("failed to lookup auth state for %s: %w", ev.ID, err)
	}
	stateMap := entries.EventIDs()
	for _, eventID := range stateMap {
		p.eventsProvider.WillGet(eventID)
	}
	return stateMap, nil
}

// txnAuthorizeAt authorizes an event against the state at a context, and returns the auth state it
// looked up there, which holds the state event the event's own tuple holds.
func (p *preparedEvents) txnAuthorizeAt(
	ctx context.Context,
	txn fdb.ReadTransaction,
	at types.StateHash,
	ev *types.Event,
) (authState types.StateMap, authErr, err error) {
	if authState, err = p.txnLookupAuthState(txn, at, ev); err != nil {
		return nil, nil, err
	}
	authErr, err = events.NewTxnAuthEventsProvider(ctx, p.eventsProvider, authState).IsEventAllowed(ev)
	return authState, authErr, err
}

// txnApplyEvent gives an authorized event its state contexts, a state event moving state on from
// before. An event that does not fit in room state fails with state.ErrEntryTooLarge.
func (p *preparedEvents) txnApplyEvent(
	txn fdb.ReadTransaction,
	before types.StateHash,
	ev *types.Event,
) error {
	after := before
	if ev.StateKey != nil {
		var err error
		if after, err = p.stateBatch.TxnApply(txn, before, types.StateEntries{ev.StateTup(): ev.StateEntry()}); errors.Is(err, state.ErrEntryTooLarge) {
			return err
		} else if err != nil {
			return fmt.Errorf("failed to apply %s to state: %w", ev.ID, err)
		} else if err := finalizeAcceptedState(p.authGraph, ev); err != nil {
			return err
		}
	}
	ev.BeforeState, ev.AfterState = before, after
	return nil
}

// txnNewStateBatch starts the state batch of a preparation, with an auth graph holding the pending
// events.
func (s *eventSender) txnNewStateBatch(
	txn fdb.ReadTransaction,
	eventsProvider *events.TxnEventsProvider,
	pending ...*types.Event,
) (*state.Batch, *events.AuthGraph) {
	authGraph := s.r.events.NewAuthGraph(txn, s.roomID, eventsProvider)
	authGraph.Add(pending...)
	return s.r.state.NewBatch(s.roomID), authGraph
}

// finalizeAcceptedState finalizes the auth graph headers of the accepted state events, which state
// contexts may hold.
func finalizeAcceptedState(authGraph *events.AuthGraph, evs ...*types.Event) error {
	var stateIDs []id.EventID
	for _, ev := range evs {
		if isAcceptedState(ev) {
			stateIDs = append(stateIDs, ev.ID)
		}
	}
	_, err := authGraph.Headers(stateIDs)
	return err
}

// txnDiffStateSteps returns the changes each step made to current state from the one before, the
// first step's from the state before the publish, or from a remote join's diffed boundary.
func (p *preparedEvents) txnDiffStateSteps(txn fdb.ReadTransaction, from types.StateHash) ([]diffedStep, error) {
	var ahead []types.StateChange
	if p.boundary != nil && len(p.stateSteps) > 0 {
		if from != p.boundary.from {
			return nil, fmt.Errorf("room state %x is not the state %x the join's state before it was diffed from", from, p.boundary.from)
		}
		from, ahead = p.boundary.state, p.boundary.changes
	}
	diffedSteps := make([]diffedStep, len(p.stateSteps))
	for i, step := range p.stateSteps {
		changes, err := p.stateBatch.TxnDiff(txn, from, step.state)
		if err != nil {
			return nil, fmt.Errorf("failed to diff room state: %w", err)
		}
		diffedSteps[i] = diffedStep{stateStep: step, changes: changes}
		from = step.state
	}
	if ahead != nil {
		diffedSteps[0].changes = p.foldStateChanges([]diffedStep{{changes: ahead}, diffedSteps[0]})
	}
	return diffedSteps, nil
}

// The walk through rejected and soft failed events reads one level of prev events at a time, and
// stops expanding past this many events, the rest counting as reached.
const maxReplacedExtremitiesWalk = 1000

// findReplacedExtremities returns the pending extremities an accepted event with prevs replaces:
// its prevs and those reached through rejected or soft failed events. Those events never became
// extremities, so the extremities behind them are still ancestors of the accepted event.
func (p *preparedEvents) findReplacedExtremities(
	log zerolog.Logger,
	pendingExtremities, prevs []id.EventID,
) ([]id.EventID, error) {
	reached := make(map[id.EventID]struct{}, len(prevs))
	for level := prevs; len(level) > 0; {
		var fetch []id.EventID
		for _, eventID := range level {
			if _, found := reached[eventID]; !found {
				reached[eventID] = struct{}{}
				fetch = append(fetch, eventID)
			}
		}
		if len(reached) > maxReplacedExtremitiesWalk {
			log.Warn().
				Int("events", len(reached)).
				Msg("Stopped walking rejected and soft failed prev events for the extremities they hide")
			break
		}

		p.eventsProvider.WillGet(fetch...)
		level = nil
		for _, eventID := range fetch {
			ev, err := p.eventsProvider.Get(eventID)
			if err != nil {
				return nil, err
			} else if ev != nil && (ev.Rejected || ev.SoftFailed) {
				level = append(level, ev.PrevEventIDs...)
			}
		}
	}

	var replaced []id.EventID
	for _, extremityID := range pendingExtremities {
		if _, found := reached[extremityID]; found {
			replaced = append(replaced, extremityID)
		}
	}
	return replaced, nil
}

// foldStateChanges is the overall change made by consecutive steps, sorted as state.Batch.TxnDiff
// sorts changes.
func (p *preparedEvents) foldStateChanges(steps []diffedStep) []types.StateChange {
	folded := make(map[types.StateTup]types.StateChange)
	for _, step := range steps {
		for _, change := range step.changes {
			if earlier, found := folded[change.StateTup]; found {
				change.OldEventID, change.OldMembership = earlier.OldEventID, earlier.OldMembership
			}
			folded[change.StateTup] = change
		}
	}
	overall := make([]types.StateChange, 0, len(folded))
	for _, change := range folded {
		if change.OldEventID != change.NewEventID {
			overall = append(overall, change)
		}
	}
	slices.SortFunc(overall, func(a, b types.StateChange) int {
		return a.Compare(b.StateTup)
	})
	return overall
}

// missingEvents returns those of the events the provider did not find
func (p *preparedEvents) missingEvents(eventIDs []id.EventID) []id.EventID {
	var missing []id.EventID
	for _, eventID := range eventIDs {
		if ev, err := p.eventsProvider.Get(eventID); err == nil && ev == nil {
			missing = append(missing, eventID)
		}
	}
	return missing
}

// Publishing

func (p *preparedEvents) sendResult(
	versionFut fdb.FutureKey,
	room *types.Room,
	changed *membershipChanges,
) *SendEventsResult {
	var change notifier.Change

	if len(p.evs) > 0 {
		change = notifier.Change{
			RoomIDs: []id.RoomID{room.ID},
			UserIDs: changed.userIDs(),
			Servers: changed.serverNames(),
			// Note: only pass the last event ID here to minimize notifier pubsub traffic
			EventIDs: []id.EventID{p.evs[len(p.evs)-1].ID},
		}
	}

	return &SendEventsResult{
		versionFut: versionFut,
		change:     change,

		Allowed:  p.evs,
		Rejected: p.rejected,
	}
}

// fitsPublish reports whether a plan's pending contexts fit the publish writing its events
func (s *eventSender) fitsPublish(plan *publishPlan) bool {
	return plan.stateBatch.PendingBytes(plan.stateContexts()...) <= s.r.config.Rooms.StateBudget.StagingBytes
}

// stageAndPublish stages the plan's contexts too large to write with its events, then publishes it.
func (s *eventSender) stageAndPublish(ctx context.Context, plan *publishPlan) (*SendEventsResult, error) {
	if !s.fitsPublish(plan) {
		if err := plan.stateBatch.Stage(ctx, s.r.config.Rooms.StateBudget.StagingBytes, plan.stateContexts()...); err != nil {
			return nil, err
		}
	}
	res, err := util.DoWriteTransactionWithVersion(ctx, s.r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		return s.txnPublish(ctx, txn, plan)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to publish events: %w", err)
	}
	return res, nil
}

// txnPublish writes a plan in one transaction once its guard holds: the stored rejected events a
// remote join accepts, the events with the rows indexing them, the room's current state with every
// row it moves, see txnStoreRoomState, and the plan's other writes.
func (s *eventSender) txnPublish(ctx context.Context, txn fdb.Transaction, plan *publishPlan) (*SendEventsResult, error) {
	room, err := s.txnCheckGuard(txn, plan)
	if err != nil {
		return nil, err
	}
	// Ahead of the state, so the publish's size bound counts them
	s.r.events.TxnAdoptEvents(txn, plan.readopted...)

	prepared := plan.preparedEvents
	prepared.eventsProvider = s.r.events.NewTxnEventsProvider(ctx, txn).WithProviderEvents(plan.eventsProvider)
	notifications := txnEvaluateNotificationsForEvents(txn, prepared.eventsProvider, prepared.evs, s.push.rules, s.push.rooms)
	changed := newMembershipChanges()
	versions, err := s.r.txnStoreEvents(ctx, txn, room, prepared.evs, prepared.replacedExtremities, notifications)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		zerolog.Ctx(ctx).Warn().Msg("No events stored in send transaction")
	} else if err := s.r.txnStoreRoomState(ctx, txn, room, &prepared, versions, changed); err != nil {
		return nil, err
	}
	if plan.resetExtremities != "" {
		s.r.events.TxnResetRoomExtremEventIDs(txn, room.ID, plan.resetExtremities)
	}
	if plan.extra != nil {
		if err := plan.extra(txn, room); err != nil {
			return nil, err
		}
	}
	return prepared.sendResult(txn.GetVersionstamp(), room, changed), nil
}

// txnCheckGuard returns the room a plan publishes to, failing with errGuardFailed unless the room is
// as the plan was prepared for and none of the auth chains and headers its graph labelled changed
// since.
func (s *eventSender) txnCheckGuard(txn fdb.Transaction, plan *publishPlan) (*types.Room, error) {
	stored, err := roomOrNil(s.r.txnGetRoom(txn, plan.room.ID))
	if err != nil {
		return nil, err
	}
	var revision uint64
	if stored != nil {
		revision = stored.StateRevision
	}
	if revision != plan.guard.revision {
		return nil, fmt.Errorf("%w: state revision %d, prepared at %d", errGuardFailed, revision, plan.guard.revision)
	} else if joined := isServerJoined(stored); joined != plan.guard.joined {
		return nil, fmt.Errorf("%w: this server joined %t, prepared %t", errGuardFailed, joined, plan.guard.joined)
	}
	if storedNow, err := s.r.txnGetStoredEventIDs(txn, plan.guard.unstored); err != nil {
		return nil, err
	} else if len(storedNow) > 0 {
		return nil, errEventsStoredConcurrently
	}
	if unchanged, err := plan.authGraph.TxnLabelsUnchanged(txn); err != nil {
		return nil, err
	} else if !unchanged {
		return nil, fmt.Errorf("%w: auth chains or headers", errEventsStoredConcurrently)
	}
	room := stored
	if room == nil {
		newRoom := *plan.room
		room = &newRoom
	}
	// A create event redacted before room version 11 lost its room version, which a remote join gives
	room.Version = cmp.Or(room.Version, plan.room.Version)
	return room, nil
}

// afterSend finishes a send that published, once the room's mutex is released, notifying of what the
// send changed.
func (s *eventSender) afterSend(ctx context.Context, res *SendEventsResult) {
	if res.transactionDuplicate {
		return
	}
	s.r.notifier.SendChange(res.change)

	rlog := zerolog.Ctx(ctx).Info().
		Object("change", res.change).
		Int("events_allowed", len(res.Allowed)).
		Int("events_rejected", len(res.Rejected))
	if len(res.Allowed) > 0 {
		rlog = rlog.Any("versionstamp", types.DecodeRawVersionstamp(res.versionFut.MustGet()))
	}
	rlog.Msg("Sent events")
}

// Preparation work
//

// preparationWork is supporting data to build before preparing again. Only artifacts have a
// stable key: staging may legitimately be requested again after a concurrent writer changed events.
type preparationWork interface {
	artifactKey() string
	fmt.Stringer
}

type artifactInputs struct {
	stateBatch     *state.Batch
	eventsProvider *events.TxnEventsProvider
	authGraph      *events.AuthGraph
}

type resolveStateWork struct {
	request     *eventsendutil.ResolutionRequest
	inputs      artifactInputs
	roomVersion string
}

func (w resolveStateWork) artifactKey() string {
	return "resolution " + w.request.Key()
}

func (w resolveStateWork) String() string {
	return w.request.String()
}

func (s *eventSender) executePreparationWork(ctx context.Context, work preparationWork) error {
	switch work := work.(type) {
	case resolveStateWork:
		zerolog.Ctx(ctx).Info().Stringer("work", work).Msg("Running artifact job, then starting again")
		return s.buildStateResolution(ctx, work)
	default:
		return fmt.Errorf("invalid preparation work: %T", work)
	}
}

// Artifact jobs

// buildStateResolution stages pending inputs, resolves using renewable read transactions,
// then stages and stores the result for the next attempt.
func (s *eventSender) buildStateResolution(ctx context.Context, work resolveStateWork) error {
	prepared := work.inputs
	if err := prepared.stateBatch.Stage(ctx, s.r.config.Rooms.StateBudget.StagingBytes, work.request.Contexts...); err != nil {
		return err
	}

	reads, err := eventsendutil.NewArtifactReads(ctx, s.r.db, s.r.events, s.roomID, prepared.eventsProvider, prepared.authGraph)
	if err != nil {
		return err
	}
	start := time.Now()
	stateBatch := s.r.state.NewJobBatch(s.roomID, reads.Read)
	resolver := reads.NewResolver(eventsendutil.ResolverOptions{
		StateBatch:  stateBatch,
		RoomID:      s.roomID,
		RoomVersion: work.roomVersion,
	})
	if err := s.buildAndStoreResolution(ctx, resolver, stateBatch, work.request.Contexts); err != nil {
		return fmt.Errorf("failed to resolve contexts: %w", err)
	}
	stateEvents, candidates := resolver.Stats()
	zerolog.Ctx(ctx).Info().
		Str("job", "resolution").
		Any("contexts", work.request.Contexts).
		Int("state_events", stateEvents).
		Int("candidates", candidates).
		Int("transactions", reads.Transactions()).
		Dur("duration", time.Since(start)).
		Msg("Resolved state")
	return nil
}

func (s *eventSender) buildAndStoreResolution(ctx context.Context, resolver *eventsendutil.Resolver, stateBatch *state.Batch, contexts []types.StateHash) error {
	result, request, err := resolver.ResolveContexts(contexts)
	if err != nil {
		return err
	}
	if request != nil {
		return errors.New("artifact job requested another resolution")
	}
	if err := stateBatch.Stage(ctx, s.r.config.Rooms.StateBudget.StagingBytes, slices.Concat(contexts, []types.StateHash{result})...); err != nil {
		return err
	}
	return stateBatch.StoreResolved(ctx, contexts, result)
}
