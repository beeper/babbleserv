package rooms

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/stateres"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Sends an event we don't actually want stored in a room, but only on the relevant users membership
// stream so they can see them, plus the raw event bytes. Simply sets a small subset of keys we
// normally set during event store. Only a local target gets a membership row, marked outlier, which
// future non-outlier memberships overwrite. An event already stored as a room event is left as it
// is, an outlier copy would take it out of room state.
func (r *RoomsDatabase) SendFederatedOutlierMembershipEvent(ctx context.Context, ev *types.Event) error {
	if ev.Type != event.StateMember {
		panic("outlier event is not a member event")
	}

	// Flag the event as an outlier so we only store it without adding to the room/state
	ev.Outlier = true

	userID := id.UserID(*ev.StateKey)
	isLocal := r.isLocalUser(userID)

	versionFut, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (fdb.FutureKey, error) {
		roomEventVersion := txn.Get(r.events.KeyForIDToVersion(ev.ID))
		var storedFut fdb.FutureByteSlice
		if isLocal {
			storedFut = txn.Get(r.users.KeyForMembership(userID, ev.RoomID))
		}
		// A normal read, so a publish to the room meanwhile conflicts with this write, which is only
		// for a room this server is not in
		room, err := roomOrNil(r.txnGetRoom(txn, ev.RoomID))
		if err != nil {
			return nil, err
		} else if isServerJoined(room) {
			return nil, fmt.Errorf("cannot send outlier events to rooms this server is participating in")
		} else if roomEventVersion.MustGet() != nil {
			return nil, nil
		}

		version := r.events.TxnStoreEventRows(txn, true, ev)[ev.ID]
		if isLocal {
			// This server is out of the room, so the user is not among its local joined members
			mtup := ev.MembershipTup()
			stored := storedFut.MustGet()
			r.users.TxnStoreMembership(txn, userID, ev.RoomID, types.MembershipRow{MembershipTup: mtup, Outlier: true})
			txn.Clear(r.keyForLocalMember(ev.RoomID, userID))
			if stored == nil || types.BytesToMembershipRow(stored).EventID != ev.ID {
				r.users.TxnStoreMembershipChange(txn, userID, version, mtup)
			}
		}

		return txn.GetVersionstamp(), nil
	})

	if err != nil {
		return err
	} else if versionFut == nil {
		zerolog.Ctx(ctx).Debug().Stringer("event_id", ev.ID).Msg("Outlier membership event is stored as a room event")
		return nil
	}
	if isLocal {
		r.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
	}
	zerolog.Ctx(ctx).Debug().
		Stringer("event_id", ev.ID).
		Stringer("target_user_id", userID).
		Stringer("sender", ev.Sender).
		Str("membership", string(ev.Membership())).
		Bool("is_outlier", ev.Outlier).
		Any("versionstamp", types.DecodeRawVersionstamp(versionFut.MustGet())).
		Msg("Stored outlier membership event")
	return nil
}

var (
	ErrAuthStage4            = errors.New("failed to auth event (step 4)")
	ErrAuthStage5            = errors.New("failed to auth event (step 5)")
	ErrEventDropped          = errors.New("event dropped")
	ErrPrevEventsUnavailable = errors.New("state at prev events unavailable")
)

// State another server gave where our state doesn't exist or is stale (we're (re)joining a room)
type GivenStates struct {
	// The state event IDs before prev events without state here, from /state_ids
	BeforePrevs map[id.EventID][]id.EventID
	// The state event IDs before batch events, as send_join gives the state before a join
	BeforeEvents map[id.EventID][]id.EventID
	// Events not stored here: the events the states name with their auth chains, and the prev
	// events. Every event a state before a batch event names must be here or stored.
	Events []*types.Event
}

// Send federated events to a room after passing through all the required
// authorization checks. (steps 4-6: https://spec.matrix.org/v1.10/server-server-api/#checks-performed-on-receipt-of-a-pdu)
//
// Every event fetched from other servers must be in evs or given: an event that cannot be evaluated
// is dropped with an error wrapping ErrEventDropped. An event with a given before-state takes it
// instead of the state after its prev events. Remote joins are passed to remoteJoinIn.
func (r *RoomsDatabase) SendFederatedEvents(
	ctx context.Context,
	roomID id.RoomID,
	evs []*types.Event,
	given *GivenStates,
	userPushRules types.UserPushRulesMap,
	userRoomContext types.UserRoomContextMap,
) (*SendEventsResult, error) {
	push := pushContext{rules: userPushRules, rooms: userRoomContext}
	p := &federatedEventSender{eventSender: r.newEventSender(roomID, push)}
	evs = p.sortEventsTopologically(evs)
	p.evs = evs
	log := r.getTxnLogContext(ctx, "SendFederatedEvents").
		Str("room_id", roomID.String()).
		Int("events", len(evs)).
		Logger()
	ctx = log.WithContext(ctx)
	if given != nil {
		p.given = givenStates{beforePrevs: given.BeforePrevs, beforeEvents: given.BeforeEvents}
		p.fetched = given.Events
	}
	p.build = p.executePreparationWork

	if join := r.remoteJoinIn(evs, given); join != nil {
		ctx, cancel := p.withSendTimeout(ctx, remoteJoinTimeout)
		defer cancel()
		p.join = join
		p.given = givenStates{beforeEvents: map[id.EventID][]id.EventID{join.ev.ID: join.stateIDs}}
		s := &remoteJoinSender{federatedEventSender: p}
		return s.send(ctx)
	}
	if joined, err := r.IsServerJoined(ctx, roomID, r.config.ServerName); err != nil {
		return nil, err
	} else if !joined {
		return r.sendOutlierMemberships(ctx, roomID, evs)
	}

	ctx, cancel := p.withSendTimeout(ctx, sendTimeout)
	defer cancel()
	res, err := p.send(ctx)
	if errors.Is(err, ErrServerNotInRoom) {
		log.Info().Err(err).Msg("This server left the room while sending events")
		return r.sendOutlierMemberships(ctx, roomID, evs)
	}
	return res, err
}

// sendOutlierMemberships stores those of a batch's events for a room this server is not in that are
// outlier memberships, see isOutlierMembership, and drops the rest with ErrServerNotInRoom.
func (r *RoomsDatabase) sendOutlierMemberships(ctx context.Context, roomID id.RoomID, evs []*types.Event) (*SendEventsResult, error) {
	res := &SendEventsResult{}
	for _, ev := range evs {
		if outlier, err := r.isOutlierMembership(ctx, roomID, ev); err != nil {
			return nil, err
		} else if !outlier {
			res.Rejected = append(res.Rejected, RejectedEvent{ev, fmt.Errorf("%w: %w: %s", ErrEventDropped, ErrServerNotInRoom, roomID)})
			continue
		}
		outlier := *ev
		if err := r.SendFederatedOutlierMembershipEvent(ctx, &outlier); err != nil {
			return nil, err
		}
		res.Allowed = append(res.Allowed, &outlier)
	}
	return res, nil
}

// isOutlierMembership reports whether a member event of a room this server is not in reaches a local
// user who sees it outside the room: an invite of a local user, or a leave answering a local user's
// invite, sent by the inviter rescinding it or by the invitee rejecting a local user's invite. A
// remote user's membership is never an outlier, as only local users have membership rows.
func (r *RoomsDatabase) isOutlierMembership(ctx context.Context, roomID id.RoomID, ev *types.Event) (bool, error) {
	if ev.Type != event.StateMember || ev.StateKey == nil || ev.RoomID != roomID {
		return false, nil
	}
	target := id.UserID(*ev.StateKey)
	if !r.isLocalUser(target) {
		return false, nil
	}
	switch ev.Membership() {
	case event.MembershipInvite:
		return true, nil
	case event.MembershipLeave:
		return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
			memberships, err := r.txnGetRoomUserMemberships(txn, roomID, []id.UserID{target})
			if err != nil {
				return false, err
			} else if membership, found := memberships[target]; !found || membership.Membership != event.MembershipInvite {
				return false, nil
			} else if invite := r.events.TxnGetEvent(txn, membership.EventID); invite == nil {
				return false, nil
			} else {
				rejects := ev.Sender == target && r.isLocalUser(invite.Sender)
				return ev.Sender == invite.Sender || rejects, nil
			}
		})
	}
	return false, nil
}

// givenStates is what a send keeps of the states the sending server gave
type givenStates struct {
	beforePrevs  map[id.EventID][]id.EventID
	beforeEvents map[id.EventID][]id.EventID
	// Before-contexts of batch events built already, by a remote join that found this server joined
	// once it had
	boundaries map[id.EventID]types.StateHash
}

// federatedEventSender prepares federated events to a room this server is joined to, once the events
// fetched for the given states are stored without state.
type federatedEventSender struct {
	eventSender
	evs     []*types.Event
	given   givenStates
	fetched []*types.Event
	// Set for a remote join, including after it switches to member mode
	join *remoteJoin
}

func (p *federatedEventSender) send(ctx context.Context) (*SendEventsResult, error) {
	return p.run(ctx, p.prepare)
}

func (p *federatedEventSender) prepare(ctx context.Context) (preparationOutcome, error) {
	evs := p.attemptEvents(p.evs)
	var dropped map[id.EventID]struct{}
	if len(p.fetched) > 0 {
		checked, err := p.checkFetchedEvents(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to check fetched events: %w", err)
		} else if len(checked.kept) > 0 {
			return needsWork{work: stageEventsWork{staging: &stagingEvents{
				evs: checked.kept, rejectedAuth: checked.rejectedAuth, joined: true,
			}}}, nil
		}
		dropped = checked.dropped
	}
	outcome, err := util.DoReadTransaction(ctx, p.r.db, func(txn fdb.ReadTransaction) (preparationOutcome, error) {
		return p.txnPrepareFederated(ctx, txn, evs, dropped)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to prepare federated events: %w", err)
	}
	return outcome, nil
}

func (s *federatedEventSender) executePreparationWork(ctx context.Context, work preparationWork) error {
	switch work := work.(type) {
	case stageEventsWork:
		if err := s.stageStatelessEvents(ctx, work.staging); err != nil {
			return fmt.Errorf("failed to stage events without state: %w", err)
		}
		return nil
	case joinStateWork:
		zerolog.Ctx(ctx).Info().Stringer("work", work).Msg("Running artifact job, then starting again")
		return s.buildJoinBoundary(ctx)
	default:
		return s.eventSender.executePreparationWork(ctx, work)
	}
}

type checkedFetchedEvents struct {
	// To store without state, in topological order
	kept    []*types.Event
	dropped map[id.EventID]struct{}
	// Stored events the check found rejected
	rejectedAuth map[id.EventID]struct{}
}

// checkFetchedEvents checks events fetched over federation (ie unknown to us, or previously rejected)
// against their own auth events.
func (s *federatedEventSender) checkFetchedEvents(ctx context.Context) (*checkedFetchedEvents, error) {
	room, err := util.DoReadTransaction(ctx, s.r.db, func(txn fdb.ReadTransaction) (*types.Room, error) {
		return s.txnGetFederatedRoom(txn, nil)
	})
	if err != nil {
		return nil, err
	} else if !isServerJoined(room) {
		return nil, fmt.Errorf("%w: %s", ErrServerNotInRoom, s.roomID)
	}
	batchIDs := make(map[id.EventID]struct{}, len(s.evs))
	for _, ev := range s.evs {
		batchIDs[ev.ID] = struct{}{}
	}
	var candidates []*types.Event
	for _, ev := range s.attemptEvents(s.fetched) {
		if _, found := batchIDs[ev.ID]; !found && ev.RoomID == room.ID {
			ev.RoomVersion = room.Version
			candidates = append(candidates, ev)
		}
	}
	unstored, _, err := s.eventsToStage(ctx, candidates, false)
	if err != nil {
		return nil, err
	}

	unstored = s.sortEventsTopologically(unstored)
	reads, err := eventsendutil.NewArtifactReads(ctx, s.r.db, s.r.events, s.roomID, nil, nil)
	if err != nil {
		return nil, err
	}
	dropped, rejectedAuth, err := s.checkOwnAuthEvents(ctx, reads, unstored, nil)
	if err != nil {
		return nil, err
	}
	kept := slices.DeleteFunc(unstored, func(ev *types.Event) bool {
		_, found := dropped[ev.ID]
		return found
	})
	return &checkedFetchedEvents{kept: kept, dropped: dropped, rejectedAuth: rejectedAuth}, nil
}

// federatedPreparation is one attempt of preparing a federated send
type federatedPreparation struct {
	*federatedEventSender
	ctx      context.Context
	txn      fdb.ReadTransaction
	plan     *publishPlan
	resolver *eventsendutil.Resolver
	// The state before batch events given one, taken instead of the state after their prev events
	givenBefore map[id.EventID]types.StateHash
	dropped     map[id.EventID]struct{}
	rejected    map[id.EventID]struct{}
}

func (p *federatedPreparation) newContextResolver() *eventsendutil.Resolver {
	return eventsendutil.NewResolver(p.txn, p.plan.eventsProvider, p.plan.authGraph, eventsendutil.ResolverOptions{
		StateBatch:     p.plan.stateBatch,
		RoomID:         p.roomID,
		RoomVersion:    p.plan.room.Version,
		MaxCandidates:  p.r.config.Rooms.StateBudget.InlineResolutionCandidates,
		MaxStateTuples: p.r.config.Rooms.StateBudget.InlineResolutionStateTuples,
	})
}

// txnPrepareFederated prepares an attempt's copies of federated events. Events are authorized
// against their own auth events, step 4, the state before them, step 5, and the room's current
// state, step 6.
func (s *federatedEventSender) txnPrepareFederated(
	ctx context.Context,
	txn fdb.ReadTransaction,
	evs []*types.Event,
	fetchedDropped map[id.EventID]struct{},
) (preparationOutcome, error) {
	join := s.join
	log := zerolog.Ctx(ctx)

	room, err := s.txnGetFederatedRoom(txn, join)
	if err != nil {
		return nil, err
	}
	if joined := isServerJoined(room); joined && join.inJoinMode() {
		return nil, errServerJoined
	} else if !joined && !join.inJoinMode() {
		return nil, fmt.Errorf("%w: %s", ErrServerNotInRoom, s.roomID)
	}
	var extremities []id.EventID
	if !join.inJoinMode() {
		extremities = s.r.events.TxnLookupCurrentRoomExtremEventIDs(txn, s.roomID)
	} else if join.diff == nil || room.CurrentState != join.diff.from {
		return needsWork{work: joinStateWork{from: room.CurrentState}}, nil
	}

	readopted, err := s.txnReadoptedEvents(ctx, txn)
	if err != nil {
		return nil, err
	}
	// Prefilled with the attempt's events, as the provider merely provides key value access without
	// any authorization
	eventsProvider := s.r.events.NewTxnEventsProvider(ctx, txn).WithEvents(readopted...).WithEvents(evs...)
	for _, ev := range evs {
		eventsProvider.WillGet(ev.AuthDependencyIDs()...)
		eventsProvider.WillGet(ev.PrevEventIDs...)
	}
	eventsProvider.WillGet(extremities...)

	stored, err := s.r.txnGetStoredEventIDs(txn, util.EventsToIDs(evs))
	if err != nil {
		return nil, err
	}
	// The events provider holds the incoming copies, so stored copies come from a provider of their own
	storedEvents := s.r.events.NewTxnEventsProvider(ctx, txn)
	for eventID := range stored {
		storedEvents.WillGet(eventID)
	}

	stateBatch, authGraph := s.txnNewStateBatch(txn, eventsProvider, evs...)
	plan := &publishPlan{
		room:  room,
		guard: sendGuard{revision: room.StateRevision, joined: !join.inJoinMode()},
		preparedEvents: preparedEvents{
			stateBatch:          stateBatch,
			authGraph:           authGraph,
			eventsProvider:      eventsProvider,
			evs:                 make([]*types.Event, 0, len(evs)),
			replacedExtremities: make(map[id.EventID][]id.EventID, len(evs)),
		},
	}
	p := &federatedPreparation{
		federatedEventSender: s,
		ctx:                  ctx,
		txn:                  txn,
		plan:                 plan,
		// Batch events citing a fetched event that could not be evaluated are dropped with it
		dropped:  maps.Clone(fetchedDropped),
		rejected: make(map[id.EventID]struct{}),
	}
	p.resolver = p.newContextResolver()
	if p.dropped == nil {
		p.dropped = make(map[id.EventID]struct{})
	}

	// Step 4: Passes authorization rules based on the event’s auth events, otherwise it is rejected.
	for _, ev := range evs {
		if _, found := stored[ev.ID]; found {
			storedEv, err := storedEvents.Get(ev.ID)
			if err != nil {
				return nil, err
			}
			// Still evaluated in turn, as events later in the batch may cite it, and they see the
			// outcome and state stored for it
			log.Warn().Stringer("event_id", ev.ID).Msg("Received duplicate event we already know about")
			ev.IsDuplicate = true
			if storedEv != nil {
				ev.CopyOutcome(storedEv)
			}
			if ev.Rejected {
				plan.rejected = append(plan.rejected, RejectedEvent{ev, types.ErrAlreadyExists})
			}
			continue
		}
		plan.guard.unstored = append(plan.guard.unstored, ev.ID)
		if err := s.r.txnCheckEventBeforeStore(txn, s.roomID, ev); err != nil {
			p.reject(ev, err)
			continue
		}
		ev.RoomVersion = room.Version
		drop, authErr, err := s.checkAuthEvents(ctx, eventsProvider, ev, p.dropped, false)
		if err != nil {
			return nil, err
		} else if drop {
			p.drop(ev, fmt.Errorf("%w: %w", ErrEventDropped, authErr))
		} else if authErr != nil {
			log.Err(authErr).Any("event", ev).Stringer("event_id", ev.ID).Msg("Failed to auth event (step 4)")
			p.reject(ev, fmt.Errorf("%w: %w", ErrAuthStage4, authErr))
			// Stored as rejected for good, unless an auth event it misses is stored meanwhile
			if errors.Is(authErr, events.ErrAuthEventMissing) {
				plan.guard.unstored = append(plan.guard.unstored, plan.missingEvents(ev.AuthDependencyIDs())...)
			}
		}
	}

	if join != nil {
		if i := slices.IndexFunc(evs, func(ev *types.Event) bool { return ev.ID == join.ev.ID }); i >= 0 && evs[i].IsDuplicate {
			if join.inJoinMode() {
				// Answered outside the room's join lock, see remoteJoinSender.send
				return nil, errJoinStored
			}
			join.stored = true
			res, err := s.txnStoredJoinResult(txn)
			return alreadySent{result: res}, err
		}
	}

	if join.inJoinMode() {
		plan.boundary = join.diff
		plan.resetExtremities = join.ev.ID
		p.givenBefore = map[id.EventID]types.StateHash{join.ev.ID: join.boundary}
	} else {
		boundaries, givenBefore, err := p.txnBoundaryContexts(evs, &s.given)
		if err != nil {
			return nil, err
		}
		p.resolver.SetBoundaries(boundaries)
		p.givenBefore = givenBefore
	}

	// Step 5: Passes authorization rules based on the state before the event, otherwise it is rejected.
	// Each event's state is the state given before it, or the state after its prev events, earlier
	// events in the batch having been assigned theirs by the time a later one cites them.
	for _, ev := range evs {
		if _, dropped := p.dropped[ev.ID]; dropped {
			continue
		} else if ev.IsDuplicate {
			plan.evs = append(plan.evs, ev)
		} else if keep, request, err := p.evaluate(ev); err != nil {
			return nil, err
		} else if request != nil {
			return p.resolutionWork(request), nil
		} else if keep {
			plan.evs = append(plan.evs, ev)
		}
	}
	if join.inJoinMode() {
		if i := slices.IndexFunc(plan.rejected, func(rejected RejectedEvent) bool {
			return rejected.Event.ID == join.ev.ID
		}); i >= 0 {
			return nil, plan.rejected[i].Error
		}
	}
	if join != nil && slices.ContainsFunc(plan.evs, func(ev *types.Event) bool { return ev.ID == join.ev.ID }) {
		plan.readopted = readopted
	}

	// Step 6: Passes authorization rules based on the current state of the room, otherwise it is
	// “soft failed”. Pending extremities start from the room's stored extremities, none for a join in
	// join mode, and advance as events are accepted. Current state is the state after them, resolved
	// when there are several.
	pendingExtremities := extremities
	current := stateSteps{current: room.CurrentState}
	for _, ev := range plan.evs {
		if ev.IsDuplicate || ev.Rejected {
			continue
		}
		evLog := log.With().
			Stringer("event_id", ev.ID).
			Stringer("type", ev.Type).
			Logger()

		var replaced []id.EventID
		if !join.inJoinMode() {
			_, authErr, err := plan.txnAuthorizeAt(ctx, txn, current.current, ev)
			if err != nil {
				return nil, err
			} else if authErr != nil {
				evLog.Err(authErr).
					Any("event", ev).
					Msg("Soft failed to auth event (step 6)")
				// Soft failed events keep their state, but stay out of room indices, extremities
				// and current state
				ev.SoftFailed = true
				continue
			}
			evLog.Trace().Msg("Event passed authorization step 6")

			if replaced, err = plan.findReplacedExtremities(evLog, pendingExtremities, ev.PrevEventIDs); err != nil {
				return nil, err
			}
		}
		pendingExtremities = s.advanceExtremities(pendingExtremities, replaced, ev.ID)
		plan.replacedExtremities[ev.ID] = replaced

		next := ev.AfterState
		if len(pendingExtremities) > 1 {
			var request *eventsendutil.ResolutionRequest
			if next, request, err = p.resolver.Resolve(pendingExtremities); err != nil {
				return nil, err
			} else if request != nil {
				return p.resolutionWork(request), nil
			}
		}
		current.advance(ev.ID, next)
		evLog.Debug().Msg("Event authorized for storage")
	}

	if len(pendingExtremities) > 1 {
		log.Warn().
			Strs("extremity_event_ids", util.StringersToStrs(pendingExtremities)).
			Msg("Room has several extremities, current state is resolved")
	}
	plan.stateSteps = current.steps
	return readyToPublish{plan: plan}, nil
}

// evaluate assigns an event its state and authorizes it against the state before it, step 5.
// Returns false for an event dropped from the batch.
func (p *federatedPreparation) evaluate(ev *types.Event) (bool, *eventsendutil.ResolutionRequest, error) {
	log := zerolog.Ctx(p.ctx).With().
		Stringer("event_id", ev.ID).
		Stringer("type", ev.Type).
		Logger()

	before, given := p.givenBefore[ev.ID]
	var knownPrevs []id.EventID
	if !given {
		// Resolving only a subset of the prev events would persist incomplete state. Leave the
		// event unstored so it can be evaluated again when state at every prev event is available,
		// from the sending server for prev events without state here.
		var unknownPrevs []id.EventID
		var err error
		if knownPrevs, unknownPrevs, err = p.resolver.PartitionPrevs(ev.PrevEventIDs); err != nil {
			return false, nil, err
		} else if len(unknownPrevs) > 0 {
			p.drop(ev, fmt.Errorf("%w: %w: %s", ErrEventDropped, ErrPrevEventsUnavailable, unknownPrevs))
			return false, nil, nil
		}
	}

	// An event is dropped with any auth event it cites, so no stored auth chain reaches an event
	// that is not stored.
	if authEventID, found := p.citedAuthEvent(ev, p.dropped); found {
		p.drop(ev, fmt.Errorf("%w: auth event %s was dropped", ErrEventDropped, authEventID))
		return false, nil, nil
	}

	if !given {
		var err error
		var request *eventsendutil.ResolutionRequest
		before, request, err = p.resolver.StateBefore(knownPrevs)
		if errors.Is(err, stateres.ErrUnsupportedAlgorithm) {
			// The only state that cannot be worked out from sound inputs, anything else is a
			// storage or consistency failure and fails the batch
			p.drop(ev, fmt.Errorf("%w: %w", ErrEventDropped, err))
			return false, nil, nil
		} else if err != nil {
			return false, nil, err
		} else if request != nil {
			return false, request, nil
		}
	}

	// Rejected events contribute nothing, so later events citing one fall through to the state
	// before it
	ev.BeforeState, ev.AfterState = before, before
	if ev.Rejected {
		return true, nil, nil
	}

	if authEventID, found := p.citedAuthEvent(ev, p.rejected); found {
		p.reject(ev, fmt.Errorf("%w: auth event %s was rejected", ErrAuthStage4, authEventID))
		return true, nil, nil
	}

	authState, authErr, err := p.plan.txnAuthorizeAt(p.ctx, p.txn, before, ev)
	if err != nil {
		return false, nil, err
	} else if authErr != nil {
		log.Err(authErr).
			Any("event", ev).
			Msg("Failed to auth event (step 5)")
		// Flag event as rejected - we still store it
		p.reject(ev, fmt.Errorf("%w: %w", ErrAuthStage5, authErr))
		return true, nil, nil
	}
	log.Trace().Msg("Event passed authorization step 5")

	var prevStateEventID id.EventID
	if ev.StateKey != nil {
		prevStateEventID = authState[ev.StateTup()]
	}
	if err := p.plan.txnApplyEvent(p.txn, before, ev); errors.Is(err, state.ErrEntryTooLarge) {
		log.Err(err).Msg("Rejecting state event too large for room state")
		p.reject(ev, fmt.Errorf("state event does not fit in room state: %w", err))
		return true, nil, nil
	} else if errors.Is(err, events.ErrAuthEventPending) {
		// Its auth chain reaches a batch event through stored events the batch order cannot see, so it
		// is left for the sending server to send again
		p.drop(ev, fmt.Errorf("%w: %w", ErrEventDropped, err))
		return false, nil, nil
	} else if errors.Is(err, events.ErrAuthEventNotFinalized) {
		log.Err(err).Msg("Rejecting state event citing an auth event that is not accepted state")
		p.reject(ev, fmt.Errorf("%w: %w", ErrAuthStage4, err))
		return true, nil, nil
	} else if err != nil {
		return false, nil, err
	} else if err := p.r.txnPreProcessEventUnsigned(p.txn, p.plan.eventsProvider, ev, prevStateEventID); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// resolutionWork captures only the preparation inputs the job needs after this transaction ends.
func (p *federatedPreparation) resolutionWork(request *eventsendutil.ResolutionRequest) needsWork {
	return needsWork{work: resolveStateWork{
		request: request,
		inputs: artifactInputs{
			stateBatch:     p.plan.stateBatch,
			eventsProvider: p.plan.eventsProvider,
			authGraph:      p.plan.authGraph,
		},
		roomVersion: p.plan.room.Version,
	}}
}

// reject flags an event rejected, it is still stored, and records why
func (p *federatedPreparation) reject(ev *types.Event, err error) {
	ev.Rejected = true
	p.rejected[ev.ID] = struct{}{}
	p.plan.rejected = append(p.plan.rejected, RejectedEvent{ev, err})
}

func (s *federatedEventSender) citedAuthEvent(ev *types.Event, eventIDs map[id.EventID]struct{}) (id.EventID, bool) {
	for _, authID := range ev.AuthEventIDs {
		if _, found := eventIDs[authID]; found {
			return authID, true
		}
	}
	return "", false
}

func (p *federatedPreparation) drop(ev *types.Event, err error) {
	zerolog.Ctx(p.ctx).Warn().
		Err(err).
		Stringer("event_id", ev.ID).
		Msg("Dropping event from batch")
	// Events citing it as a prev event find no state after it
	ev.BeforeState, ev.AfterState = types.StateHash{}, types.StateHash{}
	p.dropped[ev.ID] = struct{}{}
	// Replaces any step 4 rejection, which would imply the event is stored
	if i := slices.IndexFunc(p.plan.rejected, func(rejected RejectedEvent) bool {
		return rejected.Event == ev
	}); i >= 0 {
		p.plan.rejected[i].Error = err
	} else {
		p.plan.rejected = append(p.plan.rejected, RejectedEvent{ev, err})
	}
}

func (s *federatedEventSender) txnGetFederatedRoom(txn fdb.ReadTransaction, join *remoteJoin) (*types.Room, error) {
	room, err := s.r.txnGetRoom(txn, s.roomID)
	if errors.Is(err, types.ErrRoomNotFound) && join.inJoinMode() {
		newRoom := *join.room
		room, err = &newRoom, nil
	} else if errors.Is(err, types.ErrRoomNotFound) {
		room, err = s.r.newRoomFromEvents(s.roomID, util.EventsToPartialEvents(s.evs))
	}
	if err != nil {
		return nil, err
	} else if !room.Federated {
		return nil, errRoomNotFederated
	}
	if join != nil {
		room.Version = cmp.Or(room.Version, join.ev.RoomVersion)
	}
	return room, nil
}

// Events checked against their own auth events per batch of reads
const ownAuthCheckChunk = 1000

func (s *federatedEventSender) checkOwnAuthEvents(
	ctx context.Context,
	reads *eventsendutil.ArtifactReads,
	evs []*types.Event,
	required *types.Event,
) (dropped, rejectedAuth map[id.EventID]struct{}, err error) {
	checking := make(map[id.EventID]struct{}, len(evs))
	for _, ev := range evs {
		checking[ev.ID] = struct{}{}
	}
	if err := reads.Do(func() error {
		reads.Events().WithEvents(evs...)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	dropped = make(map[id.EventID]struct{})
	rejectedAuth = make(map[id.EventID]struct{})
	for chunk := range slices.Chunk(evs, ownAuthCheckChunk) {
		if err := reads.Do(func() error {
			eventsProvider := reads.Events()
			for _, ev := range chunk {
				eventsProvider.WillGet(ev.AuthEventIDs...)
			}
			for _, ev := range chunk {
				// A batch of reads failing runs again
				ev.Rejected = false
				delete(dropped, ev.ID)
				drop, authErr, err := s.checkAuthEvents(ctx, eventsProvider, ev, dropped, true)
				if err != nil {
					return fmt.Errorf("failed to check event %s: %w", ev.ID, err)
				} else if authErr == nil {
					continue
				} else if ev == required {
					return fmt.Errorf("%w: %s event %s: %w", ErrAuthStage4, ev.Type, ev.ID, authErr)
				}
				evLog := zerolog.Ctx(ctx).Warn().
					Err(authErr).
					Stringer("event_id", ev.ID).
					Stringer("type", ev.Type)
				if drop {
					evLog.Msg("Dropping event that could not be authorized (step 4)")
					dropped[ev.ID] = struct{}{}
					continue
				}
				evLog.Msg("Rejecting event that failed auth (step 4)")
				ev.Rejected = true
				for _, authID := range ev.AuthEventIDs {
					if _, found := checking[authID]; found {
						continue
					} else if authEv, err := eventsProvider.Get(authID); err != nil {
						return err
					} else if authEv != nil && authEv.Rejected {
						rejectedAuth[authID] = struct{}{}
					}
				}
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	return dropped, rejectedAuth, nil
}

func (s *federatedEventSender) checkAuthEvents(
	ctx context.Context,
	eventsProvider *events.TxnEventsProvider,
	ev *types.Event,
	dropped map[id.EventID]struct{},
	dropMissing bool,
) (drop bool, authErr, err error) {
	if authID, found := s.citedAuthEvent(ev, dropped); found {
		return true, fmt.Errorf("auth event %s was dropped", authID), nil
	}
	if authErr, err = eventsProvider.CheckEventAuthEvents(ctx, ev); err != nil {
		return false, nil, err
	}
	return dropMissing && errors.Is(authErr, events.ErrAuthEventMissing), authErr, nil
}

// Staging

type stageEventsWork struct{ staging *stagingEvents }

func (stageEventsWork) artifactKey() string { return "" }
func (stageEventsWork) String() string      { return "stage supporting events" }

// eventsToStage returns those of the events to stage, once each and keeping their order: those not
// stored as room events yet, and with readopt the stored ones the check did not reject that are
// stored rejected, which it also returns.
func (s *federatedEventSender) eventsToStage(
	ctx context.Context,
	evs []*types.Event,
	readopt bool,
) ([]*types.Event, map[id.EventID]struct{}, error) {
	seen := make(map[id.EventID]struct{}, len(evs))
	unique := make([]*types.Event, 0, len(evs))
	for _, ev := range evs {
		if _, found := seen[ev.ID]; !found {
			seen[ev.ID] = struct{}{}
			unique = append(unique, ev)
		}
	}
	stored, err := s.r.storedEventIDs(ctx, util.EventsToIDs(unique))
	if err != nil {
		return nil, nil, err
	}
	var candidates []*types.Event
	for _, ev := range unique {
		if _, found := stored[ev.ID]; found && readopt && !ev.Rejected {
			candidates = append(candidates, ev)
		}
	}
	rejected, err := s.storedRejected(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	staged := slices.DeleteFunc(unique, func(ev *types.Event) bool {
		_, found := stored[ev.ID]
		_, readopted := rejected[ev.ID]
		return found && !readopted
	})
	return staged, rejected, nil
}

// storedRejected returns those of the stored events whose stored copy is rejected, reading the copies
// of a chunk of them per read transaction, see storedCheckChunks.
func (s *federatedEventSender) storedRejected(ctx context.Context, evs []*types.Event) (map[id.EventID]struct{}, error) {
	rejected := make(map[id.EventID]struct{})
	for _, chunk := range s.storedCheckChunks(evs) {
		if _, err := util.DoReadTransaction(ctx, s.r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
			storedEvents := s.r.events.NewTxnEventsProvider(ctx, txn)
			storedEvents.WillGet(util.EventsToIDs(chunk)...)
			for _, ev := range chunk {
				if storedEv, err := storedEvents.Get(ev.ID); err != nil {
					return nil, err
				} else if storedEv != nil && storedEv.Rejected {
					rejected[ev.ID] = struct{}{}
				}
			}
			return nil, nil
		}); err != nil {
			return nil, err
		}
	}
	return rejected, nil
}

// Read batch size limits for previously stored events (to check current rejected state)
const (
	storedCheckChunk  = 5000
	readoptCheckBytes = 4 << 20
)

func (s *federatedEventSender) storedCheckChunks(evs []*types.Event) [][]*types.Event {
	var chunks [][]*types.Event
	size := 0
	for _, ev := range evs {
		evSize := len(ev.ToMsgpack())
		if n := len(chunks); n == 0 || len(chunks[n-1]) == storedCheckChunk || size+evSize > readoptCheckBytes {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], ev)
		size += evSize
	}
	return chunks
}

// Estimated bytes an event stored without state writes besides its eid value and the auth event IDs
// of its eah row: three keys, the etv value, chain rows and a conflict range for each row read or
// written.
const stagedEventOverhead = 1000

// stagingEvents are checked events of a room to store without state, in topological order, citing
// only stored events or earlier ones.
type stagingEvents struct {
	evs []*types.Event
	// Stored already as rejected, which a remote join's publish accepts again, see txnReadoptedEvents:
	// staging only finalizes their auth headers, as the events citing them need
	readopt map[id.EventID]struct{}
	// Stored events the check found rejected, so the events citing them were rejected
	rejectedAuth map[id.EventID]struct{}
	// Whether the events were checked for a room this server is joined to, or a remote join
	joined bool
	staged bool
}

// stageStatelessEvents stores checked events of a room without state ahead of a send
func (s *federatedEventSender) stageStatelessEvents(ctx context.Context, staging *stagingEvents) error {
	evs := staging.evs
	for _, ev := range evs {
		if _, found := staging.readopt[ev.ID]; !found {
			s.r.stripEventUnsigned(ev)
		}
	}
	staged := 0
	for _, chunk := range s.chunkStatelessEvents(evs) {
		staged += len(chunk)
		if _, err := util.DoWriteTransactionWithVersion(ctx, s.r.db, func(txn fdb.Transaction) (types.Nil, error) {
			return nil, s.txnStageStatelessEvents(ctx, txn, evs[:staged], chunk, staging)
		}); err != nil {
			return err
		}
	}
	staging.staged = true
	return nil
}

func (s *federatedEventSender) chunkStatelessEvents(evs []*types.Event) [][]*types.Event {
	budget := s.r.config.Rooms.StateBudget.StagingBytes
	var chunks [][]*types.Event
	size := 0
	for _, ev := range evs {
		evSize := s.stagedEventBytes(ev)
		if n := len(chunks); n == 0 || size+evSize > budget || len(chunks[n-1]) == types.MaxVersionstampUserVersion {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], ev)
		size += evSize
	}
	return chunks
}

func (s *federatedEventSender) stagedEventBytes(ev *types.Event) int {
	size := len(ev.ToMsgpack()) + stagedEventOverhead
	for _, authID := range ev.AuthEventIDs {
		size += len(authID)
	}
	return size
}

func (s *federatedEventSender) txnStageStatelessEvents(
	ctx context.Context,
	txn fdb.Transaction,
	staged, chunk []*types.Event,
	staging *stagingEvents,
) error {
	// A snapshot read, as a publish to the room changes nothing staging relies on. A remote join
	// finding this server joined starts again as a member's join.
	room, err := roomOrNil(s.r.txnGetRoom(txn.Snapshot(), s.roomID))
	if err != nil {
		return err
	} else if joined := isServerJoined(room); joined && !staging.joined {
		return fmt.Errorf("%w: %w", errGuardFailed, errServerJoined)
	} else if !joined && staging.joined {
		return fmt.Errorf("%w: %s", ErrServerNotInRoom, s.roomID)
	}
	var unstored []*types.Event
	var rejectedAuth []id.EventID
	for _, ev := range chunk {
		if _, found := staging.readopt[ev.ID]; !found {
			unstored = append(unstored, ev)
		}
		for _, authID := range ev.AuthEventIDs {
			if _, found := staging.rejectedAuth[authID]; found && !slices.Contains(rejectedAuth, authID) {
				rejectedAuth = append(rejectedAuth, authID)
			}
		}
	}
	if storedNow, err := s.r.txnGetStoredEventIDs(txn, util.EventsToIDs(unstored)); err != nil {
		return err
	} else if len(storedNow) > 0 {
		return errEventsStoredConcurrently
	}
	storedEvents := s.r.events.NewTxnEventsProvider(ctx, txn)
	storedEvents.WillGet(rejectedAuth...)
	for _, authID := range rejectedAuth {
		if authEv, err := storedEvents.Get(authID); err != nil {
			return err
		} else if authEv != nil && !authEv.Rejected {
			return fmt.Errorf("%w: auth event %s accepted since", errEventsStoredConcurrently, authID)
		}
	}

	// The graph reads the chain allocator and tips it extends with normal reads, so another writer
	// extending them conflicts with this one
	eventsProvider := s.r.events.NewTxnEventsProvider(ctx, txn).WithEvents(staged...)
	authGraph := s.r.events.NewAuthGraph(txn, s.roomID, eventsProvider)
	authGraph.Add(chunk...)
	if err := finalizeAcceptedState(authGraph, chunk...); err != nil {
		return err
	}
	s.r.events.TxnStoreEventRows(txn, false, unstored...)
	authGraph.Write(txn)
	return nil
}

// Join state

type joinStateWork struct{ from types.StateHash }

func (w joinStateWork) artifactKey() string { return "join " + string(w.from[:]) }
func (w joinStateWork) String() string {
	return fmt.Sprintf("the state before the join diffed from context %x", w.from)
}

func (s *federatedEventSender) txnStoredJoinResult(
	txn fdb.ReadTransaction,
) (*SendEventsResult, error) {
	joinEv := s.join.ev
	stored := s.r.events.TxnGetEvent(txn, joinEv.ID)
	if stored == nil {
		return nil, fmt.Errorf("stored remote join: %w: %s", types.ErrEventNotFound, joinEv.ID)
	}

	var rejection error
	switch {
	case stored.Rejected:
		rejection = fmt.Errorf("%w: remote join %s was rejected", types.ErrAlreadyExists, joinEv.ID)
	case stored.SoftFailed:
		rejection = fmt.Errorf("%w: remote join %s was soft failed", types.ErrAlreadyExists, joinEv.ID)
	case !stored.HasStateIn(s.roomID):
		room, err := roomOrNil(s.r.txnGetRoom(txn, s.roomID))
		if err != nil {
			return nil, err
		} else if !isServerJoined(room) {
			return nil, errJoinUnpublished
		}
		joiner := id.UserID(*joinEv.StateKey)
		current, err := s.r.txnRoomMembers(txn, room, []id.UserID{joiner})
		if err != nil {
			return nil, err
		} else if current[types.MemberStateTup(joiner)].EventID != joinEv.ID {
			rejection = fmt.Errorf("remote join %s: %w", joinEv.ID, types.ErrStateUnavailable)
		}
	}
	res := &SendEventsResult{transactionDuplicate: true}
	if rejection != nil {
		res.Rejected = []RejectedEvent{{Event: stored, Error: rejection}}
	} else {
		res.Allowed = []*types.Event{stored}
	}
	return res, nil
}

// txnReadoptedEvents returns accepted copies, without state, of events currently stored rejected
func (s *federatedEventSender) txnReadoptedEvents(
	ctx context.Context,
	txn fdb.ReadTransaction,
) ([]*types.Event, error) {
	candidates := s.join.readopted()
	if len(candidates) == 0 {
		return nil, nil
	}
	storedEvents := s.r.events.NewTxnEventsProvider(ctx, txn)
	storedEvents.WillGet(candidates...)
	var readopted []*types.Event
	for _, eventID := range candidates {
		storedEv, err := storedEvents.Get(eventID)
		if err != nil {
			return nil, err
		} else if storedEv == nil || !storedEv.Rejected {
			continue
		}
		zerolog.Ctx(ctx).Warn().
			Stringer("event_id", storedEv.ID).
			Stringer("type", storedEv.Type).
			Msg("Accepting stored rejected event from the join response")
		// A rejected event was never soft failed
		readoptedEv := *storedEv
		readoptedEv.ResetOutcome()
		readopted = append(readopted, &readoptedEv)
	}
	return readopted, nil
}

// buildJoinBoundary is the artifact job building the state before a remote join as a boundary over
// the room's current state, and diffing the current state against it for the join's publish. It
// stages what it builds.
func (s *federatedEventSender) buildJoinBoundary(ctx context.Context) error {
	join := s.join
	room, err := s.r.GetRoom(ctx, s.roomID)
	if err != nil {
		return err
	} else if room == nil {
		room = join.room
	}

	start := time.Now()
	reads, err := eventsendutil.NewArtifactReads(ctx, s.r.db, s.r.events, s.roomID, nil, nil)
	if err != nil {
		return err
	}
	stateBatch := s.r.state.NewJobBatch(s.roomID, reads.Read)
	// A job batch reads in the job's transactions whatever transaction it is given
	base, baseState, err := s.txnBoundaryBase(nil, stateBatch, room.CurrentState, len(join.state))
	if err != nil {
		return err
	}
	boundary, err := s.txnApplyBoundary(nil, stateBatch, base, baseState, join.state)
	if err != nil {
		return err
	}
	// Ahead of staging, which leaves the boundary's pages to storage the job's older transactions
	// may not see yet
	changes, err := stateBatch.TxnDiff(nil, room.CurrentState, boundary)
	if err != nil {
		return fmt.Errorf("failed to diff the room's state against the state before the join: %w", err)
	} else if err := stateBatch.Stage(ctx, s.r.config.Rooms.StateBudget.StagingBytes, boundary, base); err != nil {
		return err
	}
	zerolog.Ctx(ctx).Info().
		Str("job", "join state").
		Int("state", len(join.state)).
		Int("current_state", len(baseState)).
		Int("changes", len(changes)).
		Int("transactions", reads.Transactions()).
		Dur("duration", time.Since(start)).
		Msg("Built the state before the join")
	join.boundary = boundary
	join.diff = &boundaryChanges{from: room.CurrentState, state: boundary, changes: changes}
	return nil
}

// Event ordering

// sortEventsTopologically orders a batch so every event follows the batch events it cites as prev
// or auth events, otherwise keeping the input order, and without later copies of an event. Events
// on a cycle, which no valid batch has, follow the rest in input order.
func (s *federatedEventSender) sortEventsTopologically(batch []*types.Event) []*types.Event {
	evs := make([]*types.Event, 0, len(batch))
	indexes := make(map[id.EventID]int, len(batch))
	for _, ev := range batch {
		if _, found := indexes[ev.ID]; !found {
			indexes[ev.ID] = len(evs)
			evs = append(evs, ev)
		}
	}

	citedBy := make([][]int, len(evs))
	remaining := make([]int, len(evs))
	for i, ev := range evs {
		cited := make(map[int]struct{})
		for _, citedID := range slices.Concat(ev.PrevEventIDs, ev.AuthDependencyIDs()) {
			if j, found := indexes[citedID]; found && j != i {
				cited[j] = struct{}{}
			}
		}
		for j := range cited {
			citedBy[j] = append(citedBy[j], i)
		}
		remaining[i] = len(cited)
	}

	ready := &indexHeap{}
	for i := range evs {
		if remaining[i] == 0 {
			*ready = append(*ready, i)
		}
	}
	heap.Init(ready)
	sorted := make([]*types.Event, 0, len(evs))
	placed := make([]bool, len(evs))
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		sorted = append(sorted, evs[i])
		placed[i] = true
		for _, j := range citedBy[i] {
			remaining[j]--
			if remaining[j] == 0 {
				heap.Push(ready, j)
			}
		}
	}
	for i, ev := range evs {
		if !placed[i] {
			sorted = append(sorted, ev)
		}
	}
	return sorted
}

// indexHeap gives the lowest index first
type indexHeap []int

func (h indexHeap) Len() int           { return len(h) }
func (h indexHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h indexHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *indexHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *indexHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// advanceExtremities returns the pending extremities once an event is accepted: those it does not
// replace, then the event.
func (s *federatedEventSender) advanceExtremities(pendingExtremities, replaced []id.EventID, eventID id.EventID) []id.EventID {
	next := make([]id.EventID, 0, len(pendingExtremities)+1)
	for _, extremityID := range pendingExtremities {
		if extremityID != eventID && !slices.Contains(replaced, extremityID) {
			next = append(next, extremityID)
		}
	}
	return append(next, eventID)
}

// Given state

var errUnusableBoundary = errors.New("unusable state from the sending server")

// givenState is a state the sending server gave: the state before a prev event without state here,
// which with the prev event gives the state after it, or the state before a batch event
type givenState struct {
	eventID id.EventID
	// Nil for the state before a batch event
	prev     *types.Event
	stateIDs []id.EventID
}

// txnBoundaryContexts builds the contexts of the states the sending server gave. The state after
// each prev event without state here is built from the state before it, as Synapse does for prev
// events still missing after get_missing_events: that state, without events unknown, rejected or not
// state here, and the prev event itself when it is an accepted state event. Step 5 is not run on the
// prev event. The state before a batch event is built the same way without adding the event, unless
// built already. A state that is unusable gives no context.
func (p *federatedPreparation) txnBoundaryContexts(
	batch []*types.Event,
	given *givenStates,
) (afterPrevs, beforeEvents map[id.EventID]types.StateHash, err error) {
	// A batch event is only known to be stored once evaluated, after the boundaries are needed
	unsettled := make(map[id.EventID]struct{}, len(batch))
	for _, ev := range batch {
		if !ev.IsDuplicate {
			unsettled[ev.ID] = struct{}{}
		}
	}
	prevIDs := slices.Sorted(maps.Keys(given.beforePrevs))
	p.plan.eventsProvider.WillGet(prevIDs...)
	var states []givenState
	size := 0
	for _, prevID := range prevIDs {
		prev, err := p.plan.eventsProvider.Get(prevID)
		if err != nil {
			return nil, nil, err
		} else if _, found := unsettled[prevID]; found || prev.HasStateIn(p.plan.room.ID) {
			continue
		} else if prev == nil || prev.Outlier || prev.RoomID != p.plan.room.ID {
			zerolog.Ctx(p.ctx).Warn().Stringer("prev_event_id", prevID).Msg("No state after prev event, it is unknown")
			continue
		}
		states = append(states, givenState{eventID: prevID, prev: prev, stateIDs: given.beforePrevs[prevID]})
		// With the prev event's own tuple
		size = max(size, len(given.beforePrevs[prevID])+1)
	}
	beforeEvents = make(map[id.EventID]types.StateHash)
	for _, ev := range batch {
		stateIDs, found := given.beforeEvents[ev.ID]
		if !found || ev.IsDuplicate {
			continue
		} else if boundary, built := given.boundaries[ev.ID]; built {
			beforeEvents[ev.ID] = boundary
			continue
		}
		// As a send_join response may repeat the join
		stateIDs = slices.DeleteFunc(slices.Clone(stateIDs), func(eventID id.EventID) bool { return eventID == ev.ID })
		states = append(states, givenState{eventID: ev.ID, stateIDs: stateIDs})
		size = max(size, len(stateIDs))
	}
	if len(states) == 0 {
		return nil, beforeEvents, nil
	}

	base, baseState, err := p.txnBoundaryBase(p.txn, p.plan.stateBatch, p.plan.room.CurrentState, size)
	if err != nil {
		return nil, nil, err
	}
	baseTups := make(map[id.EventID]types.StateTup, len(baseState))
	for tup, entry := range baseState {
		baseTups[entry.EventID] = tup
	}
	for _, g := range states {
		for _, eventID := range g.stateIDs {
			if _, found := baseTups[eventID]; !found {
				p.plan.eventsProvider.WillGet(eventID)
			}
		}
	}

	afterPrevs = make(map[id.EventID]types.StateHash, len(states))
	for _, g := range states {
		log := zerolog.Ctx(p.ctx).With().Stringer("event_id", g.eventID).Bool("prev_event", g.prev != nil).Logger()
		stateMap, err := p.boundaryState(baseState, baseTups, g.prev, g.stateIDs, unsettled)
		if errors.Is(err, errUnusableBoundary) {
			log.Warn().Err(err).Msg("Unusable state given by the sending server")
			continue
		} else if err != nil {
			return nil, nil, err
		}
		boundary, err := p.txnApplyBoundary(p.txn, p.plan.stateBatch, base, baseState, stateMap)
		if errors.Is(err, state.ErrEntryTooLarge) {
			log.Warn().Err(err).Msg("Unusable state given by the sending server, a state event does not fit in room state")
			continue
		} else if err != nil {
			return nil, nil, fmt.Errorf("failed to build the state given at %s: %w", g.eventID, err)
		}
		if g.prev != nil {
			afterPrevs[g.eventID] = boundary
		} else {
			beforeEvents[g.eventID] = boundary
		}
	}
	return afterPrevs, beforeEvents, nil
}

// The state a sending server gave, with the prev event's own tuple for the state after a prev event,
// nil for the state before a batch event. Unusable when it names two events for one tuple, a batch
// event, or no create event. Every event in a context is accepted state of the room, so one in the
// base, at baseTups, is that tuple's event without loading it, with the membership the base holds.
func (p *federatedPreparation) boundaryState(
	base types.StateEntries,
	baseTups map[id.EventID]types.StateTup,
	prev *types.Event,
	stateIDs []id.EventID,
	unsettled map[id.EventID]struct{},
) (types.StateEntries, error) {
	for _, eventID := range stateIDs {
		if _, found := unsettled[eventID]; found {
			return nil, fmt.Errorf("%w: names batch event %s", errUnusableBoundary, eventID)
		}
	}
	stateMap := make(types.StateEntries, len(stateIDs))
	for _, eventID := range stateIDs {
		tup, inBase := baseTups[eventID]
		entry := base[tup]
		if !inBase {
			ev, err := p.plan.eventsProvider.Get(eventID)
			if err != nil {
				return nil, err
			} else if !p.isRoomState(ev) {
				continue
			}
			tup, entry = ev.StateTup(), ev.StateEntry()
		}
		if err := p.addGivenState(stateMap, tup, entry); err != nil {
			return nil, fmt.Errorf("%w: %w", errUnusableBoundary, err)
		}
	}
	if prev != nil && isAcceptedState(prev) {
		stateMap[prev.StateTup()] = prev.StateEntry()
	}
	// Events judged on state without one are rejected, which is stored for good
	if _, found := stateMap[types.StateTup{Type: event.StateCreate}]; !found {
		return nil, fmt.Errorf("%w: names no create event", errUnusableBoundary)
	}
	return stateMap, nil
}

// txnBoundaryBase returns the context boundaries of at most size tuples are built over, and its
// state: the room's current state, which a boundary usually shares most of its state with. Current
// state of over twice that size differs from each boundary by more tuples than the boundary holds,
// so it is not read and the base is empty.
func (s *federatedEventSender) txnBoundaryBase(
	txn fdb.ReadTransaction,
	stateBatch *state.Batch,
	currentState types.StateHash,
	size int,
) (types.StateHash, types.StateEntries, error) {
	if currentState.IsZero() {
		return state.EmptyContext, types.StateEntries{}, nil
	}
	if count, err := stateBatch.TxnCount(txn, currentState); err != nil {
		return types.StateHash{}, nil, fmt.Errorf("failed to count the room's current state: %w", err)
	} else if count > 2*size {
		return state.EmptyContext, types.StateEntries{}, nil
	}
	baseState, err := stateBatch.TxnIterateEntries(txn, currentState)
	if err != nil {
		return types.StateHash{}, nil, fmt.Errorf("failed to read the room's current state: %w", err)
	}
	return currentState, baseState, nil
}

// txnApplyBoundary builds the context holding a boundary's state from the base or from empty,
// whichever needs fewer changes. Both give the same context.
func (s *federatedEventSender) txnApplyBoundary(
	txn fdb.ReadTransaction,
	stateBatch *state.Batch,
	base types.StateHash,
	baseState, target types.StateEntries,
) (types.StateHash, error) {
	from, delta := base, state.Delta(baseState, target)
	if len(delta) > len(target) {
		from, delta = state.EmptyContext, target
	}
	return stateBatch.TxnApply(txn, from, delta)
}

// The event may be nil
func (s *federatedEventSender) isRoomState(ev *types.Event) bool {
	return ev != nil && !ev.Outlier && ev.RoomID == s.roomID && isAcceptedState(ev)
}

// A second event for one tuple is an error
func (s *federatedEventSender) addGivenState(stateMap types.StateEntries, tup types.StateTup, entry types.StateEntry) error {
	if existing, found := stateMap[tup]; found && existing.EventID != entry.EventID {
		return fmt.Errorf("names %s and %s for %v", existing.EventID, entry.EventID, tup)
	}
	stateMap[tup] = entry
	return nil
}
