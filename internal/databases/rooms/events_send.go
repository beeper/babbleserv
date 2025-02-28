// The send events transactions are the engine of Babbleserv, this contains the
// logic to authorize and ingest events from local homeserver users and events
// coming from other homeservers via federation.

package rooms

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SendEventsResult struct {
	versionstampFut fdb.FutureKey
	change          notifier.Change

	Allowed  []*types.Event
	Rejected []RejectedEvent
}

type RejectedEvent struct {
	Event *types.Event
	Error error
}

func newSendEventsResults(
	change notifier.Change,
	versionstampFut fdb.FutureKey,
	allowedEvs []*types.Event,
	rejectedEvs []RejectedEvent,
) *SendEventsResult {
	return &SendEventsResult{
		versionstampFut: versionstampFut,
		change:          change,

		Allowed:  allowedEvs,
		Rejected: rejectedEvs,
	}
}

func getUserIDList(evs []*types.PartialEvent) []id.UserID {
	userIDMap := make(map[id.UserID]struct{}, len(evs))
	for _, ev := range evs {
		userIDMap[ev.Sender] = struct{}{}
		if ev.Type == event.StateMember {
			userIDMap[id.UserID(*ev.StateKey)] = struct{}{}
		}
	}
	userIDs := make([]id.UserID, 0, len(userIDMap))
	for userID := range userIDMap {
		userIDs = append(userIDs, userID)
	}
	return userIDs
}

func partialEvents(evs []*types.Event) []*types.PartialEvent {
	partialEvs := make([]*types.PartialEvent, 0, len(evs))
	for _, ev := range evs {
		partialEvs = append(partialEvs, &ev.PartialEvent)
	}
	return partialEvs
}

func (r *RoomsDatabase) handleSendEventsResults(res *SendEventsResult, log zerolog.Logger) (*SendEventsResult, error) {
	r.notifiers.Rooms.SendChange(res.change)

	for _, r := range res.Rejected {
		log.Warn().Err(r.Error).Str("event_id", r.Event.ID.String()).Msg("Event rejected")
	}

	rlog := log.Info().
		Int("events_allowed", len(res.Allowed)).
		Int("events_rejected", len(res.Rejected))
	if len(res.Allowed) > 0 {
		rlog = rlog.Str("versionstamp", res.versionstampFut.MustGet().String())
	}
	rlog.Msg("Sent events")

	return res, nil
}

type SendLocalEventsOptions struct {
	PreloadProviders     []*events.TxnEventsProvider
	StartTransactionHook func(fdb.ReadTransaction) error
	TxnRefresh           func(fdb.Transaction)
}

// Send local events to a room, populating prev/auth events as well as authorizing
// the new events against the current room state.
func (r *RoomsDatabase) SendLocalEvents(
	ctx context.Context,
	roomID id.RoomID,
	partialEvs []*types.PartialEvent,
	options SendLocalEventsOptions,
) (*SendEventsResult, error) {
	lock, _ := r.roomLocks.GetOrSet(roomID, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	log := r.getTxnLogContext(ctx, "SendLocalEvents").
		Str("room_id", roomID.String()).
		Int("events", len(partialEvs)).
		Logger()

	ctx = log.WithContext(ctx)

	if res, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		if options.StartTransactionHook != nil {
			if err := options.StartTransactionHook(txn); err != nil {
				return nil, err
			}
		}

		if options.TxnRefresh != nil {
			options.TxnRefresh(txn)
		}

		allowedEvs, rejectedEvs, err := r.txnPrepareLocalEvents(ctx, txn, roomID, partialEvs, options)
		if err != nil {
			return nil, err
		}

		return newSendEventsResults(
			r.txnStoreEvents(ctx, txn, roomID, allowedEvs),
			txn.GetVersionstamp(),
			allowedEvs,
			rejectedEvs,
		), nil
	}); err != nil {
		return nil, err
	} else {
		return r.handleSendEventsResults(res, log)
	}
}

// Prepare, but don't send, a local event - this populates the event ID, prev/auth
// events and authenticates it against the *current* state. The resulting event
// must be sent in a separate transaction using the *SendFederatedEvents* method
// which will re-evaluate the auth rules.
// Returns (evErr, err) where evErr is a rejection error (can be returned to users)
func (r *RoomsDatabase) PrepareLocalEvents(ctx context.Context, partialEvs []*types.PartialEvent) ([]*types.Event, error, error) {
	log := r.getTxnLogContext(ctx, "PrepareLocalEvents").
		Str("room_id", partialEvs[0].RoomID.String()).
		Logger()

	ctx = log.WithContext(ctx)
	var evErr error

	evs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		allowedEvs, rejectedEvs, err := r.txnPrepareLocalEvents(
			ctx, txn, partialEvs[0].RoomID, partialEvs, SendLocalEventsOptions{},
		)
		if err != nil {
			return nil, err
		}
		for _, rEv := range rejectedEvs {
			evErr = rEv.Error
			return nil, nil
		}
		return allowedEvs, nil
	})
	if err != nil {
		return nil, nil, err
	} else if evErr != nil {
		return nil, evErr, nil
	} else {
		return evs, nil, nil
	}
}

func (r *RoomsDatabase) txnPrepareLocalEvents(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	partialEvs []*types.PartialEvent,
	options SendLocalEventsOptions,
) ([]*types.Event, []RejectedEvent, error) {
	roomKey := r.KeyForRoom(roomID)
	txn.Get(roomKey) // start fetching the room for later

	eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
	if len(options.PreloadProviders) > 0 {
		eventsProvider = eventsProvider.WithProviderEvents(options.PreloadProviders...)
	}

	// Get the current room state which we'll use to authenticate the events
	currentStateMap, err := r.events.TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
		ctx,
		txn,
		roomID,
		getUserIDList(partialEvs),
		eventsProvider,
	)

	// Check that this server is part of the room currently (or we're creating
	// a room). If not we cannot authorize & send (local) events.
	if partialEvs[0].Type != event.StateCreate &&
		!r.servers.TxnMustIsServerInRoom(txn, r.config.ServerName, roomID) {
		return nil, nil, errors.New("this server is not in this room")
	}

	// Fetch the room version from existing room or from the create event
	roomBytes := txn.Get(roomKey).MustGet()
	var roomVersion string
	var depth int64
	if roomBytes != nil {
		room := types.MustNewRoomFromBytes(roomBytes, roomID)
		roomVersion = room.Version
		depth = room.CurrentDepth
	} else {
		// If roomBytes is nil we must be creating the room, which means the first input event
		// *must* be the create event.
		rmver := gjson.GetBytes(partialEvs[0].Content, "room_version")
		if !rmver.Exists() {
			return nil, nil, errors.New("unable to determine room version")
		}
		roomVersion = rmver.String()
	}

	// TODO: prev_events cannot be unlimited size, need to cap at the most
	// recent(?) X event IDs

	// Get the current last (dangling) room events for prev_events. This may
	// be multiple events if federation is involved, due to forks in the DAG.
	// State forks cannot occur for local events thanks to FoundationDB, so
	// we do not need to resolve the state here. State resolution is only
	// needed when receiving federated events with multiple prev_events and
	// thus different states on the DAG.

	// Is this correct?
	// Current room state events cannot be updated out-of-order w/FoundationDB
	// The auth check using current state happens within the transaction

	prevEventIDs, err := r.events.TxnLookupCurrentRoomExtremEventIDs(txn, roomID)
	if err != nil {
		return nil, nil, err
	}

	authProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, currentStateMap)

	allowedEvs := make([]*types.Event, 0, len(partialEvs))
	rejectedEvs := make([]RejectedEvent, 0)

	// Send all events in the batch with the same UTC timestamp
	originTimestamp := time.Now().UTC()

	keyID, key := r.config.MustGetActiveSigningKey()

	for _, partialEv := range partialEvs {
		ev := &types.Event{PartialEvent: *partialEv}

		// Apply all the server-generated events bits before we calculate the ID
		ev.Depth = depth

		ev.Timestamp = originTimestamp.UnixMilli()
		ev.Origin = r.config.ServerName
		ev.RoomVersion = roomVersion

		ev.PrevEventIDs = prevEventIDs
		ev.AuthEventIDs = authProvider.GetAuthEventIDsForEvent(ev)

		util.HashAndSignEvent(ev, r.config.ServerName, keyID, key)

		if err := r.txnCheckEventBeforeStore(txn, roomID, ev); err != nil {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		}

		if err := authProvider.IsEventAllowed(ev); err != nil {
			zerolog.Ctx(ctx).Err(err).
				Stringer("event_id", ev.ID).
				Any("event", ev).
				Msg("Failed to auth event against current state")
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		}

		// Event is allowed, use as next prev and bump depths
		prevEventIDs = []id.EventID{ev.ID}
		depth += 1

		allowedEvs = append(allowedEvs, ev)
		eventsProvider.Add(ev)
		zerolog.Ctx(ctx).Debug().
			Str("event_id", ev.ID.String()).
			Str("type", ev.Type.String()).
			Msg("Event authorized for storage")
	}

	return allowedEvs, rejectedEvs, nil
}

type SendFederatedEventsOptions struct {
	SendLocalEventsOptions
	SkipPrevStateCheck bool
}

// Send federated events to a room after passing through all the required
// authorization checks. (steps 4-6: https://spec.matrix.org/v1.10/server-server-api/#checks-performed-on-receipt-of-a-pdu)
//
// This method assumes all remote fetching of events has been completed and are
// included in evs, any that are not will be rejected. Since this entire batch
// must be executed in a single FDB txn we only have 5s, so can't waste time
// fetching events.
func (r *RoomsDatabase) SendFederatedEvents(
	ctx context.Context,
	roomID id.RoomID,
	evs []*types.Event,
	options SendFederatedEventsOptions,
) (*SendEventsResult, error) {
	lock, _ := r.roomLocks.GetOrSet(roomID, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	log := r.getTxnLogContext(ctx, "SendFederatedEvents").
		Str("room_id", roomID.String()).
		Int("events", len(evs)).
		Logger()

	ctx = log.WithContext(ctx)
	userIDs := getUserIDList(partialEvents(evs))

	rejectedEvs := make([]RejectedEvent, 0)

	var eventsProvider *events.TxnEventsProvider
	var roomVersion string
	var err error

	// First read only transaction - pre-check room can federated, reject dupes
	// and apply the first authorization check:
	// Step 4: Passes authorization rules based on the event’s auth events, otherwise it is rejected.
	if evs, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		// Fetch the room version from existing room or from the create event,
		// since version + federation bool are immutable this check is safe to
		// perform outside of the main write transaction.
		roomBytes := txn.Get(r.KeyForRoom(roomID)).MustGet()
		if roomBytes != nil {
			room := types.MustNewRoomFromBytes(roomBytes, roomID)
			if !room.Federated {
				return nil, errors.New("this room is not federated")
			}
			roomVersion = room.Version
		} else {
			// If roomBytes is nil we must be creating the room, which means the first input event
			// *must* be the create event.
			if evs[0].Type != event.StateCreate {
				return nil, errors.New("room create event is missing, rejecting entire batch")
			}
			rmver := gjson.GetBytes(evs[0].Content, "room_version")
			if !rmver.Exists() {
				return nil, errors.New("unable to determine room version")
			}
			roomVersion = rmver.String()
		}

		// Setup our initial events provider prefilled with the input events
		// this is safe because the provider merely provides key value access
		// without any authorization.
		eventsProvider = r.events.NewTxnEventsProvider(ctx, txn).WithEvents(evs...)

		// Quickly initiate fetches of all the events direct auth+prev events
		for _, ev := range evs {
			// For each event we need it's auth events
			for _, evID := range ev.AuthEventIDs {
				eventsProvider.WillGet(evID)
			}
			// And for each event's prev events we need their auth events, first
			// grab the prev events themselves.
			for _, evID := range ev.PrevEventIDs {
				eventsProvider.WillGet(evID)
			}
		}

		// Also check for any events we already have so we can reject the dupe
		eventIDToVersionFut := make(map[id.EventID]fdb.FutureByteSlice)
		for _, ev := range evs {
			eventIDToVersionFut[ev.ID] = txn.Get(r.events.KeyForIDToVersion(ev.ID))
		}

		allowedEvs := make([]*types.Event, 0, len(evs))

		for _, ev := range evs {
			evLog := log.With().
				Str("event_id", ev.ID.String()).
				Str("type", ev.Type.String()).
				Logger()

			// Check if we already have this event - can happen if another server
			// gets confused and sends a duplicate.
			evExists, err := eventIDToVersionFut[ev.ID].Get()
			var isDuplicate bool
			if err != nil && err != types.ErrEventNotFound {
				return nil, err
			} else if evExists != nil {
				if ev.Type == event.StateMember {
					// If we're a member event it's possible the "dupe" is an
					// outlier event we're now un-outlier-ing.
					outlierKey := r.users.KeyForUserOutlierMembership(id.UserID(*ev.StateKey), ev.RoomID)
					if txn.Get(outlierKey).MustGet() == nil {
						isDuplicate = true
					}
				} else {
					isDuplicate = true
				}
			}
			if isDuplicate {
				evLog.Warn().Msg("Rejecting duplicate event we already know about")
				rejectedEvs = append(rejectedEvs, RejectedEvent{ev, types.ErrAlreadyExists})
				continue
			}

			ev.RoomVersion = roomVersion

			if err := r.txnCheckEventBeforeStore(txn, roomID, ev); err != nil {
				rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
				continue
			}

			authEvStateMap := make(types.StateMap)
			for _, authID := range ev.AuthEventIDs {
				authEv, err := eventsProvider.Get(authID)
				if err != nil {
					return nil, err
				}
				if authEv.StateKey == nil {
					// Should we reject this event if the auth events contain non-state events?
					// The spec language is "should be the following subset of the room state"
					// https://spec.matrix.org/v1.11/server-server-api/#auth-events-selection
					evLog.Warn().Msg("Ignoring non-state auth event")
					continue
				}
				authEvStateMap[types.StateTup{
					Type:     authEv.Type,
					StateKey: *authEv.StateKey,
				}] = authEv.ID
			}

			authEvAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, authEvStateMap)
			if err := authEvAuthProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Any("event", ev).
					Str("event_id", ev.ID.String()).
					Msg("Failed to auth event (step 4)")
				rejectedEvs = append(rejectedEvs, RejectedEvent{
					ev,
					fmt.Errorf("failed to auth event (step 4): %w", err),
				})
				continue
			} else {
				evLog.Trace().Msg("Event passed authorization step 4")
				allowedEvs = append(allowedEvs, ev)
			}
		}
		return allowedEvs, nil
	}); err != nil {
		return nil, err
	}

	// Second read only transaction, second authorization check:
	// Step 5: Passes authorization rules based on the state before the event, otherwise it is rejected.
	if evs, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		if options.SkipPrevStateCheck {
			return evs, nil
		}

		// New provider for this txn copying any events we pulled in the last
		eventsProvider = r.events.NewTxnEventsProvider(ctx, txn).WithProviderEvents(eventsProvider)

		// Now for each event fetch any prev event's auth events *if we already
		// have the prev event* (we often will not).
		for _, ev := range evs {
			for _, prevID := range ev.PrevEventIDs {
				// Grab the prev event, will block if not loaded yet
				prevEv, err := eventsProvider.Get(prevID)
				if err == types.ErrEventNotFound {
					// We may not have this event as it could be in the ev list,
					// so we'll do the state fetch on-demand later.
					continue
				} else if err != nil {
					return nil, err
				}
				// And the prev events auth events
				for _, pEvID := range prevEv.AuthEventIDs {
					eventsProvider.WillGet(pEvID)
				}
				// TODO: potentially kick off range fetches for the state at
				// time of prev (don't need futures, FDB client will cache)?
			}
		}

		// Keep track of state at each event as we authenticate them (including
		// the event itself) so we can use it in the prev state checks. Often
		// we'll be persisting a batch of events that refer to one another as
		// prev_events and this enables doing that.
		evIDToStateMap := make(map[id.EventID]types.StateMap, len(evs))
		getStateAtEventID := func(evID id.EventID) (types.StateMap, error) {
			if state, found := evIDToStateMap[evID]; found {
				return state, nil
			}
			return r.events.TxnLookupRoomAuthAndSpecificMemberStateMapAtEvent(
				ctx,
				txn,
				roomID,
				userIDs,
				evID,
				eventsProvider,
			)
		}

		allowedEvs := make([]*types.Event, 0, len(evs))

		for _, ev := range evs {
			evLog := log.With().
				Str("event_id", ev.ID.String()).
				Str("type", ev.Type.String()).
				Logger()

			var prevEvStateMap types.StateMap

			if len(ev.PrevEventIDs) == 1 {
				prevEvStateMap, err = getStateAtEventID(ev.PrevEventIDs[0])
				if err == types.ErrEventNotFound {
					rejectedEvs = append(rejectedEvs, RejectedEvent{
						ev,
						errors.New("failed to auth event (step 5): prev event not found"),
					})
					continue
				} else if err != nil {
					return nil, fmt.Errorf("failed to get state at prev event: %w", err)
				}
			} else {
				// We have multiple states to resolve, we need all the state events
				// (both conflicted + unconflicted).
				allStateEvents := make([]*types.Event, 0)

				for _, prevID := range ev.PrevEventIDs {
					prevStateMap, err := getStateAtEventID(prevID)
					if err == types.ErrEventNotFound {
						rejectedEvs = append(rejectedEvs, RejectedEvent{
							ev,
							fmt.Errorf("failed to auth event (step 5): %w", err),
						})
						continue
					} else if err != nil {
						return nil, fmt.Errorf("failed to get state at prev event: %w", err)
					}
					for _, stateID := range prevStateMap {
						stateEv := eventsProvider.MustGet(stateID)
						allStateEvents = append(allStateEvents, stateEv)
					}
				}

				// Finally, grab all the auth chains for all the state events we
				// need to resolve.
				allAuthEvents, err := r.events.TxnGetAuthChainForEvents(txn, allStateEvents, eventsProvider)
				if err != nil {
					return nil, err
				}

				evMap := util.MergeEventsMap(allStateEvents, allAuthEvents)
				resolvedPDUs, err := gomatrixserverlib.ResolveConflicts(
					gomatrixserverlib.RoomVersion(roomVersion),
					util.EventsToPDUs(allStateEvents),
					util.EventsToPDUs(allAuthEvents),
					func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
						return senderID.ToUserID(), nil
					},
					func(eventID string) bool {
						return evMap[id.EventID(eventID)].SoftFailed || evMap[id.EventID(eventID)].Outlier
					},
				)
				if err != nil {
					return nil, err
				}

				// Turn the resolved state back into our stateMap/memberMap
				prevEvStateMap = make(types.StateMap, len(resolvedPDUs))
				for _, pdu := range resolvedPDUs {
					pduEv := pdu.(types.EventPDU).Event()
					prevEvStateMap[types.StateTup{
						Type:     pduEv.Type,
						StateKey: *pduEv.StateKey,
					}] = pduEv.ID
				}
			}

			// Store the state map for the event now, before we finish authorizing
			// it, we'll update it with the event iself if it is allowed.
			evIDToStateMap[ev.ID] = prevEvStateMap

			prevStateAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, prevEvStateMap)
			if err := prevStateAuthProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Any("event", ev).
					Str("event_id", ev.ID.String()).
					Msg("Failed to auth event (step 5)")
				rejectedEvs = append(rejectedEvs, RejectedEvent{
					ev,
					fmt.Errorf("failed to auth event (step 5): %w", err),
				})
				continue
			} else {
				evLog.Trace().Msg("Event passed authorization step 5")
				if ev.StateKey != nil {
					// Update the state at this event to include it
					evIDToStateMap[ev.ID][ev.StateTup()] = ev.ID
				}
				allowedEvs = append(allowedEvs, ev)
			}
		}
		return allowedEvs, nil
	}); err != nil {
		return nil, err
	}

	// The actual write transaction and final authorization step:
	// Step 6: Passes authorization rules based on the current state of the room, otherwise it is “soft failed”.
	if res, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		// New provider for this txn copying any events we pulled in the last two
		eventsProvider = r.events.NewTxnEventsProvider(ctx, txn).WithProviderEvents(eventsProvider)

		// Get the current room auth state + members
		// Get the current room state which we'll use to authenticate the events
		currentStateMap, err := r.events.TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
			ctx,
			txn,
			roomID,
			userIDs,
			eventsProvider,
		)
		if err != nil {
			return nil, err
		}

		// Use this auth provider throughout as we accept events after step 6
		currentStateAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, currentStateMap)

		allowedEvs := make([]*types.Event, 0, len(evs))

		for _, ev := range evs {
			evLog := log.With().
				Str("event_id", ev.ID.String()).
				Str("type", ev.Type.String()).
				Logger()

			if err := currentStateAuthProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Any("event", ev).
					Str("event_id", ev.ID.String()).
					Msg("Soft failed to auth event (step 6)")

				// Flag the event as soft failed to exclude it from room indices
				// and current state. Otherwise should be treated as normal, so
				// from a federation perspective, it succeeded.
				ev.SoftFailed = true
			} else {
				evLog.Trace().Msg("Event passed authorization step 6")
			}

			allowedEvs = append(allowedEvs, ev)
			eventsProvider.Add(ev)
			evLog.Debug().Msg("Event authorized for storage")
		}

		return newSendEventsResults(
			r.txnStoreEvents(ctx, txn, roomID, allowedEvs),
			txn.GetVersionstamp(),
			allowedEvs,
			rejectedEvs,
		), nil
	}); err != nil {
		return nil, err
	} else {
		return r.handleSendEventsResults(res, log)
	}
}

func (r *RoomsDatabase) SendFederatedOutlierMembershipEvent(ctx context.Context, ev *types.Event) error {
	if ev.Type != event.StateMember {
		panic("outlier event is not a member event")
	}

	// Flag the event as an outlier so we only store it without adding to the room/state
	ev.Outlier = true

	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*struct{}, error) {
		r.txnStoreEvents(ctx, txn, ev.RoomID, []*types.Event{ev})

		// Store user/room -> MembershipTup
		txn.Set(
			r.users.KeyForUserOutlierMembership(id.UserID(*ev.StateKey), ev.RoomID),
			types.MembershipTupToValue(ev.MembershipTup()),
		)

		return nil, nil
	})
	return err
}

// Run final internal checks on an event before we accept it for storage, this
// is where we prevent duplicate annotations currently.
func (r *RoomsDatabase) txnCheckEventBeforeStore(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	ev *types.Event,
) error {
	// Quick sanity checks - should never happen!
	if ev.RoomID != roomID {
		panic("event room ID does not match input room ID")
	} else if ev.RoomVersion == "" {
		panic("event room version is missing")
	}

	relEvID, relType := ev.RelatesTo()
	if relType == event.RelAnnotation {
		existng := txn.Get(r.events.KeyForRoomReaction(ev.RoomID, relEvID, ev.Sender, ev.ReactionKey())).MustGet()
		if existng != nil {
			return errors.New("duplicate reaction for this event/user")
		}
	}

	return nil
}

// Store events handles writing out all the relevant event data into FoundationDB
// assuming that all events passed in are already authenticated.
func (r *RoomsDatabase) txnStoreEvents(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	evs []*types.Event,
) notifier.Change {
	if len(evs) == 0 {
		return notifier.Change{}
	}

	// Grab the room object, which may be nil if not created yet
	roomKey := r.KeyForRoom(roomID)
	roomBytes := txn.Get(roomKey).MustGet()
	var room *types.Room
	var roomChanged bool
	if roomBytes == nil {
		room = &types.Room{}
	} else {
		room = types.MustNewRoomFromBytes(roomBytes, roomID)
	}

	changedUsers := make(map[id.UserID]struct{}, 0)
	changedServers := make(map[string]struct{}, 0)

	for i, ev := range evs {
		zerolog.Ctx(ctx).Trace().
			Str("event_id", ev.ID.String()).
			Str("type", ev.Type.String()).
			Msg("Storing event")

		eventIDBytes := []byte(ev.ID)

		// Firstly, store the event itself
		txn.Set(r.events.KeyForEvent(ev.ID), ev.ToMsgpack())

		// This is the magic FDB version which is globally ordered, we index
		// events by this.
		version := tuple.IncompleteVersionstamp(uint16(i))

		// Store version -> (event_id, room_id)
		txn.SetVersionstampedKey(
			r.events.KeyForVersion(version),
			types.EventIDTupToValue(ev.EventIDTup()),
		)

		if ev.Outlier {
			// If we're an outlier event, we're not part of the DAG and just need
			// to store basics, so we're done.
			continue
		}

		if ev.SoftFailed {
			// We can't just point soft failed events at the new version we're
			// about to create because they aren't valid at that point. They are
			// valid at their prev events, however, so we point their version to
			// the first one of those. This means we resolve the correct historical
			// state at a soft failed event.
			firstPrevVersion := r.events.TxnMustLookupVersionForEventID(txn, ev.PrevEventIDs[0])
			txn.SetVersionstampedValue(
				r.events.KeyForIDToVersion(ev.ID),
				types.VersionstampToValue(firstPrevVersion),
			)
			// Now we've stored the event, global version index and it's own version
			// we're done here, since soft failed events don't appear to clients.
			continue
		}

		// Store event_id -> version
		txn.SetVersionstampedValue(r.events.KeyForIDToVersion(ev.ID), types.VersionstampToValue(version))

		// Room indices
		// room/version -> event_id, used to sync room events to clients
		txn.SetVersionstampedKey(r.events.KeyForRoomVersion(ev.RoomID, version), eventIDBytes)
		// add to the room super stream
		r.txnAddEventToSuperStream(txn, ev, version)

		// State events indices
		if ev.StateKey != nil {
			if r.updateRoomForStateEvent(txn, roomID, room, ev) {
				roomChanged = true
			}

			// room/version -> StateTupWithID
			txn.SetVersionstampedKey(
				r.events.KeyForRoomStateVersion(ev.RoomID, version),
				types.StateTupWithIDToValue(ev.StateTupWithID()),
			)

			// room/type/state_key/version -> event_id
			txn.SetVersionstampedKey(
				r.events.KeyForRoomVersionStateTup(ev.RoomID, ev.Type, *ev.StateKey, version),
				eventIDBytes,
			)

			if ev.Type != event.StateMember {
				// Current (non member) room/type/state_key -> event_id
				txn.Set(r.events.KeyForRoomCurrentStateTup(ev.RoomID, ev.Type, ev.StateKey), eventIDBytes)
			} else {
				// Member event handling involves a bunch more steps, we need to
				// keep track of which users, and servers, are in the room.
				memberID := id.UserID(*ev.StateKey)
				changedUsers[memberID] = struct{}{}

				membershipTupValue := types.MembershipTupToValue(ev.MembershipTup())

				// Current room/member -> MembershipTup
				txn.Set(r.events.KeyForCurrentRoomMember(ev.RoomID, memberID), membershipTupValue)

				// Current user/room_id -> MembershipTup
				txn.Set(r.users.KeyForUserMembership(memberID, ev.RoomID), membershipTupValue)

				// User user/member_changes/version -> MembershipTup
				txn.Set(r.users.KeyForUserMembershipChange(memberID, version), membershipTupValue)

				// If this event was an outlier membership clear that for the user
				outlierKey := r.users.KeyForUserOutlierMembership(id.UserID(*ev.StateKey), ev.RoomID)
				txn.Clear(outlierKey)

				// Handle server room memberships (including local!)
				username, serverName, _ := memberID.Parse()
				serverJoinedMemberKey := r.servers.KeyForServerJoinedMember(ev.RoomID, serverName, username)

				if ev.Membership() == event.MembershipJoin {
					// If we're joining, first check if the server is currently
					// a member of this room.
					wasServerJoined := r.servers.TxnMustIsServerInRoom(txn, serverName, ev.RoomID)
					// Set the joined member key and the server membership
					txn.Set(serverJoinedMemberKey, []byte{})
					if !wasServerJoined {
						changedServers[serverName] = struct{}{}
						// Current room/server -> MembershipTup
						txn.Set(r.events.KeyForCurrentRoomServer(ev.RoomID, serverName), membershipTupValue)
						// Server name/room_id -> MembershipTup
						txn.Set(r.servers.KeyForServerMembership(serverName, ev.RoomID), membershipTupValue)
						// Server name/member_changes/version -> MembershipTup
						txn.Set(r.servers.KeyForServerMembershipChange(serverName, version), membershipTupValue)
					}
				} else {
					txn.Clear(serverJoinedMemberKey)
					// If leaving, now we've cleared the specific member key
					// we check if the server has any other joined members.
					stillJoined := len(txn.GetRange(
						r.servers.RangeForServerJoinedMembers(ev.RoomID, serverName),
						fdb.RangeOptions{
							Limit: 1,
						},
					).GetSliceOrPanic()) > 0
					if !stillJoined {
						changedServers[serverName] = struct{}{}
						// Clear current room/server, server membership and set change
						txn.Clear(r.events.KeyForCurrentRoomServer(ev.RoomID, serverName))
						txn.Clear(r.servers.KeyForServerMembership(serverName, ev.RoomID))
						txn.Set(
							r.servers.KeyForServerMembershipChange(serverName, version),
							// Note any non-join membership is handled here so we
							// create a new leave MembershipTup.
							types.MembershipTupToValue(types.MembershipTup{
								EventID:    ev.ID,
								RoomID:     ev.RoomID,
								Membership: event.MembershipLeave,
							}),
						)
					}
				}
			}
		}

		// Relation events indices
		relEvID, relType := ev.RelatesTo()
		if relEvID != "" {
			txn.Set(
				r.events.KeyForRoomRelation(ev.RoomID, relEvID, version),
				tuple.Tuple{ev.ID.String(), []byte(relType)}.Pack(),
			)

			if relType == event.RelThread {
				// room-threads/root-ev-version -> root event ID - only if this
				// doesn't already exist (so the first reply in a thread creates).
				relEvVersion := r.events.TxnMustLookupVersionForEventID(txn, relEvID)
				threadKey := r.events.KeyForRoomThread(ev.RoomID, relEvVersion)
				if txn.Get(threadKey).MustGet() == nil {
					txn.Set(threadKey, []byte(relEvID))
				}
			}

			if relType == event.RelAnnotation {
				// room-ev-reactions/rel-ev/uid/key
				// Note: dupe check is handled before we call storeEvents
				txn.Set(r.events.KeyForRoomReaction(ev.RoomID, relEvID, ev.Sender, ev.ReactionKey()), eventIDBytes)
			}
		}

		// Update room extremeties
		// This is where we handle the partial DAG ordering via prev_events
		// For each new event:
		//     store room/last/event_id empty key
		//     for each ev.prev_events, clear room/last/prev_ev_id
		// The contents of room/last/ are used at event creation time to populate
		// prev_events, thus any DAG split can be corrected by sending an event.
		for _, prevEventID := range ev.PrevEventIDs {
			txn.Clear(r.events.KeyForRoomExtrem(ev.RoomID, prevEventID))
		}
		// Set this last, so rooms always have a last event
		txn.Set(r.events.KeyForRoomExtrem(ev.RoomID, ev.ID), nil)
	}

	if roomChanged {
		txn.Set(roomKey, room.ToMsgpack())
	}

	changedUserIDs := make([]id.UserID, 0, len(changedUsers))
	for uid := range changedUsers {
		changedUserIDs = append(changedUserIDs, uid)
	}
	changedServerNames := make([]string, 0, len(changedServers))
	for serverName := range changedServers {
		if serverName != r.config.ServerName {
			changedServerNames = append(changedServerNames, serverName)
		}
	}
	return notifier.Change{
		RoomIDs: []id.RoomID{roomID},
		UserIDs: changedUserIDs,
		Servers: changedServerNames,
		// Note: only pass the last event ID here to minimize notifier pubsub traffic
		EventIDs: []id.EventID{evs[len(evs)-1].ID},
	}
}

func (r *RoomsDatabase) updateRoomForStateEvent(
	txn fdb.Transaction,
	roomID id.RoomID,
	room *types.Room,
	ev *types.Event,
) bool {
	if room.ID == "" {
		if ev.Type != event.StateCreate {
			panic("no room create event")
		}
		room.ID = roomID

		room.Version = gjson.GetBytes(ev.Content, "room_version").String()
		room.Type = gjson.GetBytes(ev.Content, "type").String()

		res := gjson.GetBytes(ev.Content, "m\\.federate")
		var canFederate bool
		if res.Exists() {
			canFederate = res.Bool()
		} else {
			canFederate = true
		}
		room.Federated = canFederate

		return true
	}

	if ev.Type == event.StateCreate {
		panic("room already exists")
	}

	var changed bool

	if ev.Depth > room.CurrentDepth {
		// TODO: move current depth to it's own dedicated key to avoid rewriting
		// the room every event.
		room.CurrentDepth = ev.Depth
		changed = true
	}

	switch ev.Type {
	case event.StateRoomName:
		room.Name = gjson.GetBytes(ev.Content, "name").String()
		changed = true
	case event.StateTopic:
		room.Topic = gjson.GetBytes(ev.Content, "topic").String()
		changed = true
	case event.StateRoomAvatar:
		room.AvatarURL = gjson.GetBytes(ev.Content, "url").String()
		changed = true
	case event.StateMember:
		membership := ev.Membership()
		wasJoined := r.users.TxnMustIsUserInRoom(txn, id.UserID(*ev.StateKey), roomID)
		switch membership {
		case event.MembershipJoin:
			if !wasJoined {
				room.MemberCount += 1
			}
		case event.MembershipLeave, event.MembershipBan:
			if wasJoined {
				room.MemberCount -= 1
			}
		}
		changed = true
	}

	// TODO: canonical alias check
	// ensure we don't conflict with another, set if not set

	return changed
}
