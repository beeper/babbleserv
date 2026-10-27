package rooms

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
)

var (
	ErrServerNotInRoom        = errors.New("this server is not in the room")
	ErrProfileUpdateNotJoined = errors.New("profile update from a user who is not joined")
)

type SendLocalEventsOptions struct {
	// Called last in every transaction attempt, eg to refresh a lock held in another database
	BeforeCommit func()

	TransactionDevice   *types.UserDevice
	TransactionEndpoint string
	TransactionID       string

	PublishRoom      bool
	RoomAlias        id.RoomAlias
	RoomAliasOwner   id.UserID
	RequireAllEvents bool

	// Records the room the send creates as BabbleBot's room with this user, see txnClaimBotRoom
	BotRoomUser id.UserID

	// The ingest job's join target the send stands in for, which its publish takes, failing with
	// ErrIngestTargetGone once another runner of the room's job failed or replaced it
	IngestJoinTarget id.EventID
}

// RequiredEventRejectedError fails a send requiring all its events once one is rejected, storing
// none of them
type RequiredEventRejectedError struct {
	Event *types.Event
	Err   error
}

func (e *RequiredEventRejectedError) Error() string {
	return fmt.Sprintf("required %s event rejected: %s", e.Event.Type, e.Err)
}

func (e *RequiredEventRejectedError) Unwrap() error { return e.Err }

// Send local events to a room, populating prev/auth events as well as authorizing the new events
// against the current room state. A room that exists takes local events only while this server is
// joined to it, failing with ErrServerNotInRoom otherwise. The events are prepared and published in
// one write transaction, unless their contexts need staging, when they are sent in attempts.
func (r *RoomsDatabase) SendLocalEvents(
	ctx context.Context,
	roomID id.RoomID,
	partialEvs []*types.PartialEvent,
	userPushRules types.UserPushRulesMap,
	userRoomContext types.UserRoomContextMap,
	options SendLocalEventsOptions,
) (*SendEventsResult, error) {
	log := r.getTxnLogContext(ctx, "SendLocalEvents").
		Str("room_id", roomID.String()).
		Int("events", len(partialEvs)).
		Logger()
	ctx = log.WithContext(ctx)

	s := &localEventSender{
		eventSender: r.newEventSender(roomID, pushContext{rules: userPushRules, rooms: userRoomContext}),
		partialEvs:  partialEvs,
		options:     options,
		now:         time.Now().UTC(),
	}
	return s.send(ctx)
}

// localEventSender keeps the send's timestamp across the fast path and every staged attempt.
type localEventSender struct {
	eventSender
	partialEvs []*types.PartialEvent
	options    SendLocalEventsOptions
	now        time.Time
}

func (s *localEventSender) send(ctx context.Context) (*SendEventsResult, error) {
	res, err := s.sendInOneTransaction(ctx)
	if errors.Is(err, errNeedsStaging) {
		zerolog.Ctx(ctx).Debug().Msg("Local events need staging, sending them in attempts")
		ctx, cancel := s.withSendTimeout(ctx, sendTimeout)
		defer cancel()
		return s.run(ctx, s.prepare)
	} else if err != nil {
		return nil, fmt.Errorf("failed to send local events: %w", err)
	}
	s.after(ctx, res)
	return res, nil
}

func (s *localEventSender) sendInOneTransaction(ctx context.Context) (*SendEventsResult, error) {
	defer s.lock()()
	return util.DoWriteTransactionWithVersion(ctx, s.r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		sender := s.eventSender
		sender.publish = func(ctx context.Context, plan *publishPlan) (*SendEventsResult, error) {
			if !s.fitsPublish(plan) {
				return nil, errNeedsStaging
			}
			return s.txnPublish(ctx, txn, plan)
		}
		res, work, err := sender.attempt(ctx, func(ctx context.Context) (preparationOutcome, error) {
			return s.txnPrepareLocal(ctx, txn)
		})
		if err == nil && work != nil {
			return nil, errNeedsStaging
		}
		return res, err
	})
}

func (s *localEventSender) prepare(ctx context.Context) (preparationOutcome, error) {
	return util.DoReadTransaction(ctx, s.r.db, func(txn fdb.ReadTransaction) (preparationOutcome, error) {
		return s.txnPrepareLocal(ctx, txn)
	})
}

func (s *localEventSender) txnPrepareLocal(ctx context.Context, txn fdb.ReadTransaction) (preparationOutcome, error) {
	options := s.options
	withTransactionID := options.TransactionDevice != nil && options.TransactionID != ""
	if withTransactionID {
		if existing := s.r.txnGetEventForTransaction(txn, *options.TransactionDevice, s.roomID, options.TransactionEndpoint, options.TransactionID); existing != nil {
			return alreadySent{result: &SendEventsResult{Allowed: []*types.Event{existing}, transactionDuplicate: true}}, nil
		}
	}

	room, err := s.r.txnGetOrCreateRoomForEvents(txn, s.roomID, s.partialEvs)
	if err != nil {
		return nil, err
	}
	created := room.StateRevision == 0
	if !created && !isServerJoined(room) {
		return nil, fmt.Errorf("%w: %s", ErrServerNotInRoom, s.roomID)
	}
	prepared, work, err := s.txnPrepareLocalEvents(ctx, txn, room, true)
	if err != nil {
		return nil, err
	} else if work != nil {
		return needsWork{work: work}, nil
	} else if options.RequireAllEvents && len(prepared.rejected) > 0 {
		return nil, &RequiredEventRejectedError{Event: prepared.rejected[0].Event, Err: prepared.rejected[0].Error}
	}
	plan := &publishPlan{
		room:           room,
		guard:          sendGuard{revision: room.StateRevision, joined: !created},
		preparedEvents: *prepared,
	}
	plan.extra = func(txn fdb.Transaction, room *types.Room) error {
		if options.RoomAlias != "" {
			if err := s.r.txnSetRoomAlias(txn, options.RoomAlias, room.ID, options.RoomAliasOwner); err != nil {
				return err
			}
		}
		if options.PublishRoom {
			s.r.txnSetRoomPublished(txn, room, true)
		}
		if options.IngestJoinTarget != "" && len(plan.evs) > 0 {
			if err := s.r.txnTakeIngestJoinTarget(txn, room.ID, options.IngestJoinTarget); err != nil {
				return err
			}
		}
		if options.BotRoomUser != "" {
			if err := s.r.txnClaimBotRoom(txn, options.BotRoomUser, room.ID); err != nil {
				return err
			}
		}
		if withTransactionID && len(plan.evs) > 0 {
			s.r.txnStoreEventTransaction(txn, *options.TransactionDevice, room.ID, options.TransactionEndpoint, options.TransactionID, plan.evs[0].ID)
		}
		if options.BeforeCommit != nil {
			options.BeforeCommit()
		}
		return nil
	}
	return readyToPublish{plan: plan}, nil
}

// Prepare, but don't send, a local event - this populates the event ID, prev/auth events and
// authenticates it against the *current* state. The resulting event must be sent in a separate
// transaction using the *SendFederatedEvents* method which will re-evaluate the auth rules.
// Returns the events and those rejected, whose errors can be returned to users.
func (r *RoomsDatabase) PrepareLocalEvents(ctx context.Context, roomID id.RoomID, partialEvs []*types.PartialEvent) ([]*types.Event, []RejectedEvent, error) {
	log := r.getTxnLogContext(ctx, "PrepareLocalEvents").
		Str("room_id", roomID.String()).
		Logger()

	ctx = log.WithContext(ctx)
	s := &localEventSender{
		eventSender: r.newEventSender(roomID, pushContext{}),
		partialEvs:  partialEvs,
		now:         time.Now().UTC(),
	}
	for built := make(builtArtifacts); ; {
		var work preparationWork
		prepared, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*preparedEvents, error) {
			room, err := r.txnGetOrCreateRoomForEvents(txn, roomID, partialEvs)
			if err != nil {
				return nil, err
			}
			var prepared *preparedEvents
			prepared, work, err = s.txnPrepareLocalEvents(ctx, txn, room, false)
			return prepared, err
		})
		if err != nil {
			return nil, nil, err
		} else if work == nil {
			return prepared.evs, prepared.rejected, nil
		} else if err := built.check(work); err != nil {
			return nil, nil, err
		} else if err := s.build(ctx, work); err != nil {
			return nil, nil, err
		}
		built.add(work)
	}
}

// txnPrepareLocalEvents builds local events sent at now in a line, each authorized against the state
// after the one before, from the room's current state. In a state DAG room each cites the state
// extremities as they stand after the events before it, and is authorized against the state after
// them. When there are too many to cite, that is not current state, and the event must pass current
// state too. A resolution too large to run here is returned as work instead. Unless publishing,
// current state is only resolved when an event needs it.
func (s *localEventSender) txnPrepareLocalEvents(
	ctx context.Context,
	txn fdb.ReadTransaction,
	room *types.Room,
	publishing bool,
) (*preparedEvents, preparationWork, error) {
	eventsProvider := s.r.events.NewTxnEventsProvider(ctx, txn)
	stateBatch, authGraph := s.txnNewStateBatch(txn, eventsProvider)
	prepared := &preparedEvents{
		stateBatch:     stateBatch,
		authGraph:      authGraph,
		eventsProvider: eventsProvider,
	}
	// Prefetch the batch's auth and previous state together. Each event below reads
	// its own before-state, including changes accepted earlier in this batch.
	lookupKeys := make(map[types.StateTup]struct{})
	for _, partialEv := range s.partialEvs {
		ev := &types.Event{PartialEvent: *partialEv, RoomVersion: room.Version}
		for _, key := range stateLookupKeys(ev) {
			lookupKeys[key] = struct{}{}
		}
	}
	initialState, err := stateBatch.TxnLookupEntries(txn, room.CurrentState, slices.Collect(maps.Keys(lookupKeys)))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to lookup current auth state: %w", err)
	}
	for _, entry := range initialState {
		eventsProvider.WillGet(entry.EventID)
	}

	var depth int64
	depthBytes := txn.Get(s.r.KeyForRoomDepth(room.ID)).MustGet()
	if depthBytes != nil {
		depth = types.BytesToRoomDepth(depthBytes) + 1
	}

	// TODO: prev_events cannot be unlimited size, need to cap at the most
	// recent(?) X event IDs, as state DAG rooms do

	// Get the current last (dangling) room events for prev_events. This may
	// be multiple events if federation is involved, due to forks in the DAG.
	// State forks cannot occur for local events thanks to FoundationDB, so
	// we do not need to resolve the state here. State resolution is only
	// needed when receiving federated events with multiple prev_events and
	// thus different states on the DAG.

	prevEventIDs := s.r.events.TxnLookupCurrentRoomExtremEventIDs(txn, room.ID)

	stateDAGs := util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs)
	var stateExtremities []id.EventID
	var resolver *eventsendutil.Resolver
	if stateDAGs {
		stateExtremities = s.r.events.TxnLookupCurrentRoomStateExtremEventIDs(txn, room.ID)
		eventsProvider.WillGet(citedExtremities(stateExtremities)...)
		if len(prevEventIDs) > types.MaxStateDAGPrevEvents {
			prevEventIDs = s.r.events.TxnOrderByStoredVersion(txn, prevEventIDs)
		}
		prevEventIDs = citedExtremities(prevEventIDs)
		resolver = s.newResolver(txn, prepared, room.Version)
	}

	allowedEvs := make([]*types.Event, 0, len(s.partialEvs))
	rejectedEvs := make([]RejectedEvent, 0)

	keyID, key := s.r.config.MustGetActiveSigningKey()

	// The room's current state after the events accepted so far, the state before the next unless
	// it cites other than every state extremity. Zero while a resolution is put off.
	steps := stateSteps{current: room.CurrentState}
	currentState := func() (types.StateHash, *eventsendutil.ResolutionRequest, error) {
		if steps.current.IsZero() {
			current, request, err := resolver.Resolve(stateExtremities)
			if err != nil || request != nil {
				return types.StateHash{}, request, err
			}
			steps.current = current
		}
		return steps.current, nil, nil
	}

	for _, partialEv := range s.partialEvs {
		ev := newLocalEvent(partialEv, room.Version, depth, prevEventIDs, s.now)

		before := steps.current
		var citesOther bool
		if stateDAGs {
			superseded, request, err := s.txnSupersededMembership(txn, stateBatch, ev, currentState)
			if err != nil || request != nil {
				return nil, s.workFor(prepared, request, room), err
			}
			cite := func(membership id.EventID) (request *eventsendutil.ResolutionRequest, err error) {
				var citesCurrent bool
				ev.PrevStateEventIDs, citesCurrent = citedPrevStateEvents(stateExtremities, membership)
				citesOther, before = !citesCurrent, steps.current
				if citesOther || before.IsZero() {
					if before, request, err = resolver.Resolve(ev.PrevStateEventIDs); err == nil && request == nil && citesCurrent {
						steps.current = before
					}
				}
				return request, err
			}
			if request, err := cite(superseded); err != nil || request != nil {
				return nil, s.workFor(prepared, request, room), err
			} else if superseded != "" && !slices.Contains(citedExtremities(stateExtremities), superseded) {
				// Resolution orders a membership behind the extremities by more than ancestry, so it
				// can come out after later state, and the rescind would make older state current
				// everywhere. Sent without the citation, the target's server may refuse it.
				current, request, err := currentState()
				if err != nil || request != nil {
					return nil, s.workFor(prepared, request, room), err
				} else if before != current {
					zerolog.Ctx(ctx).Warn().
						Stringer("superseded_event_id", superseded).
						Str("state_key", *ev.StateKey).
						Msg("Rescind citing the membership it ends would change current state, citing the state extremities alone")
					if request, err := cite(""); err != nil || request != nil {
						return nil, s.workFor(prepared, request, room), err
					}
				}
			}
		}
		beforeState, err := prepared.txnLookupAuthState(txn, before, ev)
		if err != nil {
			return nil, nil, err
		}
		if ev.Type == event.StateMember {
			var senderMember *types.Event
			if senderMemberID, found := beforeState[types.MemberStateTup(ev.Sender)]; found {
				if senderMember, err = eventsProvider.GetRequired(senderMemberID); err != nil {
					return nil, nil, err
				}
			}
			if err := s.checkBabbleProfileUpdateValid(ev, senderMember); err != nil {
				rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
				continue
			}
		}
		var prevStateEventID id.EventID
		if ev.StateKey != nil {
			stateTup := ev.StateTup()
			prevStateEventID = beforeState[stateTup]
			// Membership events are not deduped: make_join and friends build templates from
			// them, and repeated leaves or kicks are authorized afresh.
			if prevStateEventID != "" && partialEv.Type != event.StateMember {
				currentEv := eventsProvider.MustGet(prevStateEventID)
				if currentEv != nil && signedjson.Matches(partialEv.Content, currentEv.Content) {
					zerolog.Ctx(ctx).Warn().
						Stringer("event_id", currentEv.ID).
						Stringer("type", currentEv.Type).
						Str("state_key", stateTup.StateKey).
						Msg("Received duplicate state event, returning current")
					// Flag a copy as dupe (so we don't store) and return as the event, the current
					// event may be earlier in this batch and still needs storing.
					duplicateEv := *currentEv
					duplicateEv.IsDuplicate = true
					allowedEvs = append(allowedEvs, &duplicateEv)
					continue
				}
			}
		}

		ev.AuthEventIDs = events.AuthEventIDsFor(ev, beforeState)
		if err := util.HashAndSignEvent(ev, s.r.config.ServerName, keyID, key); err != nil {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		} else if err := s.r.txnCheckEventBeforeStore(txn, room.ID, ev); err != nil {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		} else if err := s.r.txnCheckDuplicateReaction(txn, ev); err != nil {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		}

		// Holds by construction while events/srx and current state hold only accepted state events
		if authErr, err := util.AuthorizePrevStateEvents(ev, eventsProvider.Get); err != nil {
			return nil, nil, err
		} else if authErr != nil {
			zerolog.Ctx(ctx).Err(authErr).Stringer("event_id", ev.ID).Msg("Local event cites invalid state extremities")
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, authErr})
			continue
		}
		if authErr, err := events.NewTxnAuthEventsProvider(ctx, eventsProvider, beforeState).IsEventAllowed(ev); err != nil {
			return nil, nil, err
		} else if authErr != nil {
			zerolog.Ctx(ctx).Err(authErr).
				Stringer("event_id", ev.ID).
				Any("event", ev).
				Msg("Failed to auth event against current state")
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, authErr})
			continue
		}
		// Other servers soft-fail a message current state refuses, such as one from a user banned on
		// a branch it does not cite. State events are refused here too, though others would take them.
		if citesOther {
			current, request, err := currentState()
			if err != nil || request != nil {
				return nil, s.workFor(prepared, request, room), err
			} else if _, authErr, err := prepared.txnAuthorizeAt(ctx, txn, current, ev); err != nil {
				return nil, nil, err
			} else if authErr != nil {
				zerolog.Ctx(ctx).Err(authErr).Stringer("event_id", ev.ID).Msg("Local event citing other than every state extremity fails current state")
				rejectedEvs = append(rejectedEvs, RejectedEvent{ev, authErr})
				continue
			}
		}

		// The graph finalizes its auth header, and later events of the batch read it as auth state
		authGraph.Add(ev)
		eventsProvider.Add(ev)
		if err := prepared.txnApplyEvent(txn, before, ev); errors.Is(err, state.ErrEntryTooLarge) {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		} else if err != nil {
			return nil, nil, err
		} else if err := s.r.txnPreProcessEventUnsigned(txn, eventsProvider, ev, prevStateEventID); err != nil {
			return nil, nil, err
		}

		// Event is allowed, use as next prev and bump depths
		prevEventIDs = []id.EventID{ev.ID}
		depth += 1
		if !stateDAGs {
			steps.advance(ev.ID, ev.AfterState)
		} else if entersStateDAG(ev) {
			stateExtremities = advanceStateExtremities(stateExtremities, ev)
			next := ev.AfterState
			if len(stateExtremities) > 1 {
				next = types.StateHash{}
				if publishing {
					var request *eventsendutil.ResolutionRequest
					if next, request, err = resolver.Resolve(stateExtremities); err != nil || request != nil {
						return nil, s.workFor(prepared, request, room), err
					}
				}
			}
			steps.advance(ev.ID, next)
		}
		allowedEvs = append(allowedEvs, ev)

		zerolog.Ctx(ctx).Debug().
			Stringer("event_id", ev.ID).
			Stringer("type", ev.Type).
			Msg("Event authorized for storage")
	}

	prepared.evs, prepared.rejected = allowedEvs, rejectedEvs
	if publishing {
		prepared.stateSteps = steps.steps
	}
	prepared.setReplacedExtremities()
	return prepared, nil, nil
}

// txnSupersededMembership returns the invite or knock in current state that a leave of a state DAG
// room rescinds out of band: the target's server is not in the room, so holds that membership
// without the state DAG, and can tell the leave supersedes it only should the leave cite it
// (MSC4242). Other changes of a remote user's membership cite the state extremities alone.
func (s *localEventSender) txnSupersededMembership(
	txn fdb.ReadTransaction,
	stateBatch *state.Batch,
	ev *types.Event,
	currentState func() (types.StateHash, *eventsendutil.ResolutionRequest, error),
) (id.EventID, *eventsendutil.ResolutionRequest, error) {
	if !changesRemoteMembership(ev, s.r.config.ServerName) || ev.Membership() != event.MembershipLeave {
		return "", nil, nil
	}
	target := id.UserID(*ev.StateKey)
	if count, err := txn.Get(s.r.servers.KeyForJoinedCount(s.roomID, target.Homeserver())).Get(); err != nil || servers.JoinedCountOf(count) > 0 {
		return "", nil, err
	}
	current, request, err := currentState()
	if err != nil || request != nil || current.IsZero() {
		return "", request, err
	}
	tup := types.MemberStateTup(target)
	entries, err := stateBatch.TxnLookupEntries(txn, current, []types.StateTup{tup})
	if err != nil {
		return "", nil, err
	}
	return rescindedMembership(entries[tup]), nil, nil
}

func (s *localEventSender) workFor(prepared *preparedEvents, request *eventsendutil.ResolutionRequest, room *types.Room) preparationWork {
	if request == nil {
		return nil
	}
	return prepared.resolutionWork(request, room.Version)
}

func (s *localEventSender) checkBabbleProfileUpdateValid(ev, senderMember *types.Event) error {
	if !ev.IsBabbleProfileUpdate() || senderMember != nil && senderMember.Membership() == event.MembershipJoin {
		return nil
	}
	return ErrProfileUpdateNotJoined
}

func newLocalEvent(
	partialEv *types.PartialEvent,
	roomVersion string,
	depth int64,
	prevEventIDs []id.EventID,
	now time.Time,
) *types.Event {
	partial := *partialEv
	if !util.RoomVersionHas(roomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) || partial.Type != event.StateCreate || partial.Timestamp == 0 {
		partial.Timestamp = now.UnixMilli()
	}
	partial.Unsigned = maps.Clone(partialEv.Unsigned)
	return &types.Event{
		PartialEvent: partial,
		Local:        true,
		Depth:        depth,
		RoomVersion:  roomVersion,
		PrevEventIDs: prevEventIDs,
	}
}

func (p *preparedEvents) setReplacedExtremities() {
	p.replacedExtremities = make(map[id.EventID][]id.EventID, len(p.evs))
	for _, ev := range p.evs {
		if !ev.IsDuplicate {
			p.replacedExtremities[ev.ID] = ev.PrevEventIDs
		}
	}
}
