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
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

type SendLocalEventsOptions struct {
	// Ensure (+refresh) a lock is held at commit time
	LockTxnRefresh lock.LockTxnRefreshFunc
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

	if res, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		room, err := r.txnGetOrCreateRoomForEvents(txn, roomID, partialEvs)
		if err != nil {
			return nil, err
		}

		allowedEvs, rejectedEvs, err := r.txnPrepareLocalEvents(ctx, txn, room, partialEvs, options)
		if err != nil {
			return nil, err
		}

		changedUsers := make(map[id.UserID]struct{}, 1)
		changedServers := make(map[string]struct{}, 1)

		if !r.txnStoreEvents(ctx, txn, room, allowedEvs, changedUsers, changedServers) {
			log.Warn().Msg("No events stored in send transaction")
		}

		if options.LockTxnRefresh != nil {
			options.LockTxnRefresh(txn)
		}

		return newSendEventsResults(
			txn.GetVersionstamp(),
			room,
			allowedEvs,
			rejectedEvs,
			changedUsers,
			changedServers,
		), nil
	}); err != nil {
		return nil, fmt.Errorf("failed to send local events: %w", err)
	} else {
		return r.handleSendEventsResults(res, log)
	}
}

// Prepare, but don't send, a local event - this populates the event ID, prev/auth
// events and authenticates it against the *current* state. The resulting event
// must be sent in a separate transaction using the *SendFederatedEvents* method
// which will re-evaluate the auth rules.
// Returns (evErr, err) where evErr is a rejection error (can be returned to users)
func (r *RoomsDatabase) PrepareLocalEvents(ctx context.Context, roomID id.RoomID, partialEvs []*types.PartialEvent) ([]*types.Event, []RejectedEvent, error) {
	log := r.getTxnLogContext(ctx, "PrepareLocalEvents").
		Str("room_id", roomID.String()).
		Logger()

	ctx = log.WithContext(ctx)
	var allowed []*types.Event
	var rejected []RejectedEvent

	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		room, err := r.txnGetOrCreateRoomForEvents(txn, roomID, partialEvs)
		if err != nil {
			return nil, err
		}
		allowed, rejected, err = r.txnPrepareLocalEvents(
			ctx, txn, room, partialEvs, SendLocalEventsOptions{},
		)
		return nil, err
	})

	return allowed, rejected, err
}

func (r *RoomsDatabase) txnPrepareLocalEvents(
	ctx context.Context,
	txn fdb.ReadTransaction,
	room *types.Room,
	partialEvs []*types.PartialEvent,
	options SendLocalEventsOptions,
) ([]*types.Event, []RejectedEvent, error) {
	eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)

	// Get the current room state which we'll use to authenticate the events
	currentStateMap := r.events.TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
		ctx,
		txn,
		room.ID,
		getUserIDList(partialEvs),
		eventsProvider,
	)

	var depth int64
	depthBytes := txn.Get(r.KeyForRoomDepth(room.ID)).MustGet()
	if depthBytes != nil {
		depth = types.BytesToRoomDepth(depthBytes) + 1
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

	prevEventIDs := r.events.TxnLookupCurrentRoomExtremEventIDs(txn, room.ID)

	authProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, currentStateMap)

	allowedEvs := make([]*types.Event, 0, len(partialEvs))
	rejectedEvs := make([]RejectedEvent, 0)

	// Send all events in the batch with the same UTC timestamp
	originTimestamp := time.Now().UTC()

	keyID, key := r.config.MustGetActiveSigningKey()

	for _, partialEv := range partialEvs {
		if partialEv.StateKey != nil {
			// If we're a state event check for any current state that is identical (by type, key
			// and content), and dedupe.
			stateTup := types.StateTup{Type: partialEv.Type, StateKey: *partialEv.StateKey}
			currentStateMap := r.events.TxnLookupCurrentStateEventIDs(txn, room.ID, []types.StateTup{stateTup}, eventsProvider)
			if evID, ok := currentStateMap[stateTup]; ok {
				currentEv := eventsProvider.MustGet(evID)
				if currentEv != nil && util.CompareSignedJSON(partialEv.Content, currentEv.Content) {
					zerolog.Ctx(ctx).Warn().
						Stringer("event_id", currentEv.ID).
						Stringer("type", currentEv.Type).
						Str("state_key", stateTup.StateKey).
						Msg("Received duplicate state event, returning current")
					// Flag as dupe (so we don't store) and return as the event
					currentEv.IsDuplicate = true
					allowedEvs = append(allowedEvs, currentEv)
					continue
				}
			}
		}

		partialEv.Timestamp = originTimestamp.UnixMilli()
		ev := &types.Event{
			PartialEvent: *partialEv,
			Local:        true,
			Depth:        depth,
			RoomVersion:  room.Version,
			PrevEventIDs: prevEventIDs,
		}

		ev.AuthEventIDs = authProvider.GetAuthEventIDsForEvent(ev)

		if err := util.HashAndSignEvent(ev, r.config.ServerName, keyID, key); err != nil {
			rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
			continue
		} else if err := r.txnCheckEventBeforeStore(txn, room.ID, ev); err != nil {
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
			Stringer("event_id", ev.ID).
			Stringer("type", ev.Type).
			Msg("Event authorized for storage")
	}

	return allowedEvs, rejectedEvs, nil
}

// Sends an event we don't actually want stored in a room, but only on the relevant users membership
// stream so they can see them, plus the raw event bytes. Simply sets a small subset of keys we
// normally set during event store. Future non-outlier memberships will overwrite.
func (r *RoomsDatabase) SendFederatedOutlierMembershipEvent(ctx context.Context, ev *types.Event) error {
	if ev.Type != event.StateMember {
		panic("outlier event is not a member event")
	}

	// Flag the event as an outlier so we only store it without adding to the room/state
	ev.Outlier = true

	userID := id.UserID(*ev.StateKey)

	versionFut, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (fdb.FutureKey, error) {
		// Important to check that we're not in the room inside the write txn
		if r.servers.TxnIsServerJoinedRoom(txn, r.config.ServerName, ev.RoomID) {
			return nil, fmt.Errorf("cannot send outlier events to rooms this server is participating in")
		}

		eventTupBytes := types.EventTupToBytes(ev.EventTup())

		// Firstly, store the event itself
		r.events.TxnStoreEvent(txn, ev)

		// Store global version -> EventTup, this is our index of all events
		version := tuple.IncompleteVersionstamp(0)
		txn.SetVersionstampedKey(r.events.KeyForVersion(version), eventTupBytes)

		// Store the membership and membership change for the user
		mtup := ev.MembershipTup()
		r.users.TxnStoreMembership(txn, userID, ev.RoomID, mtup)
		r.users.TxnStoreMembershipChange(txn, userID, version, mtup)

		return txn.GetVersionstamp(), nil
	})

	if err == nil {
		r.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
		zerolog.Ctx(ctx).Debug().
			Stringer("event_id", ev.ID).
			Stringer("target_user_id", userID).
			Stringer("sender", ev.Sender).
			Str("membership", string(ev.Membership())).
			Bool("is_outlier", ev.Outlier).
			Any("versionstamp", types.DecodeRawVersionstamp(versionFut.MustGet())).
			Msg("Stored outlier membership event")
	}
	return err
}

type SendFederatedEventsOptions struct {
	// Skip stage 5 auth of the state at each events prev_events. This is required when doing remote
	// joins where we don't have the history of the room prior to the join.
	RemoteJoinEventID id.EventID
	// By default we check, within the write txn, that this server is currently in the room - this
	// disables that when expected (remote join).
	SkipServerInRoomCheck bool
}

var (
	ErrAuthStage4 = errors.New("failed to auth event (step 4)")
	ErrAuthStage5 = errors.New("failed to auth event (step 5)")
)

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

	userIDs := getUserIDList(util.EventsToPartialEvents(evs))
	rejectedEvs := make([]RejectedEvent, 0)

	var eventsProvider *events.TxnEventsProvider
	var err error
	var room *types.Room

	// First read only transaction - pre-check room can federated, reject dupes
	// and apply the first authorization check:
	// Step 4: Passes authorization rules based on the event’s auth events, otherwise it is rejected.
	if _, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		room, err = r.txnGetOrCreateRoomForEvents(txn, roomID, util.EventsToPartialEvents(evs))
		if err != nil {
			return nil, err
		}
		if !room.Federated {
			return nil, errors.New("this room is not federated")
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

		for _, ev := range evs {
			evLog := log.With().
				Str("event_id", ev.ID.String()).
				Str("type", ev.Type.String()).
				Logger()

			// Check if we already have this event - can happen if another server
			// gets confused and sends a duplicate.
			evExists, err := eventIDToVersionFut[ev.ID].Get()
			var isDuplicate bool
			if err != nil {
				return nil, err
			} else if evExists != nil {
				if ev.Type == event.StateMember {
					// We might be un-outlier-ing a federated member event
					currentEv := r.events.TxnGetEvent(txn, ev.ID)
					if !currentEv.Outlier {
						isDuplicate = true
					}
				} else {
					isDuplicate = true
				}
			}
			if isDuplicate {
				// Flag as a dupe - but we'll still continue to process/auth it below as events
				// later in the batch may rely on it if is state.
				evLog.Warn().Msg("Received duplicate event we already know about")
				ev.IsDuplicate = true
				currentEv := eventsProvider.MustGet(ev.ID)
				if currentEv.Rejected {
					// Make sure we match the dupe
					ev.Rejected = true
					rejectedEvs = append(rejectedEvs, RejectedEvent{ev, types.ErrAlreadyExists})
				}
				continue
			} else if err := r.txnCheckEventBeforeStore(txn, roomID, ev); err != nil {
				ev.Rejected = true
				rejectedEvs = append(rejectedEvs, RejectedEvent{ev, err})
				continue
			}

			ev.RoomVersion = room.Version

			var authEventsMissing bool
			var authErr error
			authEvStateMap := make(types.StateMap)

			for _, authID := range ev.AuthEventIDs {
				authEv, err := eventsProvider.Get(authID)
				if err != nil {
					return nil, fmt.Errorf("failed to fetch auth event: %w", err)
				} else if authEv == nil || authEv.Rejected {
					// If auth event missing or rejected: go straight to fail
					authEventsMissing = true
					authErr = fmt.Errorf("auth event missing or rejected: %s", authID)
					break
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

			if !authEventsMissing {
				authEvAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, authEvStateMap)
				authErr = authEvAuthProvider.IsEventAllowed(ev)
			}
			if authEventsMissing || authErr != nil {
				log.Err(authErr).
					Any("event", ev).
					Str("event_id", ev.ID.String()).
					Msg("Failed to auth event (step 4)")
				// Flag event as rejected - we still store it
				ev.Rejected = true
				rejectedEvs = append(rejectedEvs, RejectedEvent{
					ev,
					fmt.Errorf("%w: %w", ErrAuthStage4, authErr),
				})
			} else {
				evLog.Trace().Msg("Event passed authorization step 4")
			}
		}
		return nil, nil
	}); err != nil {
		return nil, err
	}

	// Second read only transaction, second authorization check:
	// Step 5: Passes authorization rules based on the state before the event, otherwise it is rejected.
	if _, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		if options.RemoteJoinEventID != "" {
			return nil, nil
		}

		// New provider for this txn copying any events we pulled in the last
		eventsProvider = r.events.NewTxnEventsProvider(ctx, txn).WithProviderEvents(eventsProvider)

		// Now for each event fetch any prev event's auth events *if we already have the prev
		// event* (we often will not).
		for _, ev := range evs {
			if ev.Rejected {
				continue
			}
			for _, prevID := range ev.PrevEventIDs {
				// Grab the prev event, will block if not loaded yet
				prevEv, err := eventsProvider.Get(prevID)
				if err != nil {
					return nil, fmt.Errorf("failed to fetch prev event")
				} else if prevEv == nil {
					// We may not have this event as it could be in the ev list,
					// so we'll do the state fetch on-demand later.
					continue
				}
				// And the prev events auth events
				for _, pEvID := range prevEv.AuthEventIDs {
					eventsProvider.WillGet(pEvID)
				}
				// TODO: potentially kick off range fetches for the state at
				// time of prev (don't need futures, FDB client will cache)?
			}
		}

		// Keep track of state at each event as we authenticate them (including the event itself)
		// so we can use it in the prev state checks. Often we'll be persisting a batch of events
		// that refer to one another as prev_events and this enables doing that.
		evIDToStateMap := make(map[id.EventID]types.StateMap, len(evs))
		getStateAtEvent := func(evID id.EventID) types.StateMap {
			if sMap, found := evIDToStateMap[evID]; found {
				return sMap
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

		for _, ev := range evs {
			evLog := log.With().
				Stringer("event_id", ev.ID).
				Stringer("type", ev.Type).
				Logger()

			var prevEvStateMap types.StateMap
			if len(ev.PrevEventIDs) == 1 {
				if sMap, found := evIDToStateMap[ev.PrevEventIDs[0]]; found {
					prevEvStateMap = sMap
				} else {
					prevEvStateMap = getStateAtEvent(ev.PrevEventIDs[0])
				}
			} else {
				// We have multiple prev events, so we need to get the state at each, combine them
				// together and perform state resolution to get the state we need.
				stateEvs := make(map[id.EventID]struct{})
				for _, evID := range ev.PrevEventIDs {
					sMap, found := evIDToStateMap[evID]
					if !found {
						sMap = getStateAtEvent(evID)
					}
					for _, evID := range sMap {
						stateEvs[evID] = struct{}{}
					}
				}
				prevEvStateMap, err = r.txnResolveStateForEvents(txn, stateEvs, room.Version, eventsProvider)
				if err != nil {
					return nil, fmt.Errorf("failed to get state at prev event(s): %w", err)
				}
			}

			// If we've no previous state and we're not creating the room - fail
			if len(prevEvStateMap) == 0 && ev.Type != event.StateCreate {
				evLog.Error().
					Any("event", ev).
					Stringer("event_id", ev.ID).
					Any("prev_event_ids", ev.PrevEventIDs).
					Msg("Failed to auth event (step 5): prev event(s) not found")
				ev.Rejected = true
				rejectedEvs = append(rejectedEvs, RejectedEvent{
					ev,
					fmt.Errorf("failed to auth event (step 5): prev event(s) not found: %s", ev.PrevEventIDs),
				})
				continue
			}

			// Store the state map for the event now, before we finish authorizing it, we'll update
			// it with the event iself if it is allowed.
			evIDToStateMap[ev.ID] = prevEvStateMap

			// Important that we skip rejected *after* fetching any prev state, if a later event
			// refers to the rejected event as it's prev we must use the prior state.
			if ev.Rejected {
				continue
			}

			prevStateAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, prevEvStateMap)
			if err := prevStateAuthProvider.IsEventAllowed(ev); err != nil {
				evLog.Err(err).
					Any("event", ev).
					Stringer("event_id", ev.ID).
					Msg("Failed to auth event (step 5)")
				// Flag event as rejected - we still store it
				ev.Rejected = true
				rejectedEvs = append(rejectedEvs, RejectedEvent{
					ev,
					fmt.Errorf("%w: %w", ErrAuthStage5, err),
				})
				continue
			} else {
				evLog.Trace().Msg("Event passed authorization step 5")
				if ev.StateKey != nil {
					// Update the state at this event to include it
					evIDToStateMap[ev.ID][ev.StateTup()] = ev.ID
				}
			}
		}
		return nil, nil
	}); err != nil {
		return nil, err
	}

	// The actual write transaction and final authorization step:
	// Step 6: Passes authorization rules based on the current state of the room, otherwise it is “soft failed”.
	// Bonus: we must also handle state resolution here if the room now has multiple extremeties, as
	// this affects the current room state.
	if res, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (*SendEventsResult, error) {
		thisServerInRoom := r.servers.TxnIsServerJoinedRoom(txn, r.config.ServerName, roomID)
		// Important to check that we're in the room inside the write txn
		if !options.SkipServerInRoomCheck && !thisServerInRoom {
			return nil, fmt.Errorf("cannot send federated events to rooms this server is not participating in")
		}

		// New provider for this txn copying any events we pulled in the last two
		eventsProvider = r.events.NewTxnEventsProvider(ctx, txn).WithProviderEvents(eventsProvider)

		// Get the current room auth state + members
		// Get the current room state which we'll use to authenticate the events
		currentStateMap := r.events.TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
			ctx,
			txn,
			roomID,
			userIDs,
			eventsProvider,
		)

		// Use this auth provider throughout as we accept events after step 6
		currentStateAuthProvider := events.NewTxnAuthEventsProvider(ctx, eventsProvider, currentStateMap)

		for _, ev := range evs {
			if ev.Rejected {
				continue
			}
			evLog := log.With().
				Str("event_id", ev.ID.String()).
				Str("type", ev.Type.String()).
				Logger()

			if err := currentStateAuthProvider.IsEventAllowed(ev); err != nil {
				evLog.Err(err).
					Any("event", ev).
					Str("event_id", ev.ID.String()).
					Msg("Soft failed to auth event (step 6)")
				// Flag the event as soft failed to exclude it from room indices and current state
				ev.SoftFailed = true
			} else {
				evLog.Trace().Msg("Event passed authorization step 6")
			}

			eventsProvider.Add(ev)
			evLog.Debug().Msg("Event authorized for storage")
		}

		changedUsers := make(map[id.UserID]struct{}, 1)
		changedServers := make(map[string]struct{}, 1)

		if !r.txnStoreEvents(ctx, txn, room, evs, changedUsers, changedServers) {
			log.Warn().Msg("No events stored in send transaction")
		} else {
			if options.RemoteJoinEventID != "" && !thisServerInRoom {
				// We're joining the room for the first time, so overwrite the room extremeties to
				// the join event just created via the make/send federation handshake.
				r.events.TxnResetRoomExtremEventIDs(txn, roomID, options.RemoteJoinEventID)
			} else {
				// We're not joining (or already were) - check if we need to perform state res
				if err := r.txnResolveRoomState(ctx, txn, room, evs, changedUsers, changedServers, eventsProvider); err != nil {
					return nil, fmt.Errorf("failed to resolve room state: %w", err)
				}
			}
		}

		return newSendEventsResults(
			txn.GetVersionstamp(),
			room,
			evs,
			rejectedEvs,
			changedUsers,
			changedServers,
		), nil
	}); err != nil {
		return nil, fmt.Errorf("failed to send federated events: %w", err)
	} else {
		return r.handleSendEventsResults(res, log)
	}
}

// Perform state resolution on a room if needed, called whenever persisting federated events.
// Resolution is needed whenever:
// - find extremities with different IDs vs. the last time we did state res
// - if >1 of those, multiple forks have changed and we need to resolve the state
// To resolve the state, take:
// - current state
// - state *at* the time of last resolution
// - state *changes* from the time of last resolution to now
// This captures all the state events that could potentially alter the state of the room. We pass
// them all into the state resolution algorithm (from gomatrixserverlib) to get the resolved state.
// Finally we save the new resolved state position with the new extremity list.
func (r *RoomsDatabase) txnResolveRoomState(
	ctx context.Context,
	txn fdb.Transaction,
	room *types.Room,
	evs []*types.Event,
	changedUsers map[id.UserID]struct{},
	changedServers map[string]struct{},
	eventsProvider *events.TxnEventsProvider,
) error {
	log := zerolog.Ctx(ctx)

	// Now we check the room extremeties to see if we need to resolve forked states
	extremEventIDs := r.events.TxnLookupCurrentRoomExtremEventIDs(txn, room.ID)
	if len(extremEventIDs) == 1 {
		// Room has one extremety in the DAG (allowedEvs[-1]) so current state is correct
		return nil
	}

	lastResolvedIDsKey := r.events.KeyForRoomLastResolvedExtrems(room.ID)
	lastResolvedVersionKey := r.events.KeyForRoomLastResolvedVersion(room.ID)

	var prevResVersion tuple.Versionstamp

	// Now check if we've resolved state before, get the IDs and version of that
	prevResolvedExtremBytes := txn.Get(lastResolvedIDsKey).MustGet()
	if prevResolvedExtremBytes == nil {
		// Initial case: we've never done state res on this room, meaning this batch of events is
		// responsible for forking the state. So grab the state *before* this batch by using the
		// first prev event of the first event. This might over-fetch by being too old but that's
		// absolutely fine, the resolution algorithm will handle it.
		oldestEventID := evs[0].PrevEventIDs[0] // this should, by definition, be outside the batch
		prevResVersion = r.events.TxnLookupVersionForEventID(txn, oldestEventID)
	} else {
		// We have resolved state in the room before - we now check which extremity event IDs we did
		// that for. If there's only one different one in the current set we can skip state res as
		// only one fork has any changes, thus there's nothing to resolve.
		prevResolvedExtremIDs, _ := tuple.Unpack(prevResolvedExtremBytes)

		var changed int
		for _, evIDAny := range prevResolvedExtremIDs {
			evID := id.EventID(evIDAny.(string))
			for _, currentID := range extremEventIDs {
				if evID != currentID {
					changed += 1
				}
			}
		}
		if changed <= 1 {
			zerolog.Ctx(ctx).Debug().Msg("Room extremities resolved, skipping state resolution")
			return nil
		}

		prevResVersion = types.MustBytesToVersionstamp(txn.Get(lastResolvedVersionKey).MustGet())
	}

	log.Warn().
		Strs("extreme_event_ids", util.StringersToStrs(extremEventIDs)).
		Msg("Room has unresolved extremeties, performing state resolution")

	// Collect events: current
	currentStateMap := r.events.TxnLookupCurrentRoomStateAndMemberMap(txn, room.ID, eventsProvider)

	// State at point of last res
	stateAtLastResMap := r.events.TxnLookupRoomStateAndMemberMapAtVersion(txn, room.ID, prevResVersion, eventsProvider)
	// All state changes from last res -> now, this will include events from all forks (that passed
	// authorization).
	stateChangesSinceLastRes := r.events.TxnPaginateRoomStateEventTups(txn, room.ID, types.PaginationOptions{
		From: prevResVersion,
		To:   util.TxnGetLatestWriteVersion(txn),
		Mode: fdb.StreamingModeWantAll,
	}, eventsProvider)

	// Combine, resolve
	eventMap := make(map[id.EventID]struct{}, len(currentStateMap)+len(stateAtLastResMap)+len(stateChangesSinceLastRes))
	for _, evID := range currentStateMap {
		eventMap[evID] = struct{}{}
	}
	for _, evID := range stateAtLastResMap {
		eventMap[evID] = struct{}{}
	}
	for _, tup := range stateChangesSinceLastRes {
		eventMap[tup.EventID] = struct{}{}
	}
	resolvedStateMap, err := r.txnResolveStateForEvents(txn, eventMap, room.Version, eventsProvider)
	if err != nil {
		return fmt.Errorf("failed to resolve state for events: %w", err)
	}

	// Apply changes
	// Now find differences between the current state and the resolved current state, and update
	// the relevant current state keys. Note we're not sending any new events here (so clients
	// will not get these changes: https://github.com/matrix-org/matrix-spec/issues/1209). This
	// is only to ensure that incoming new events are authenticated against this resolved state.
	toClear := make([]types.EventStateTup, 0)
	toSet := make([]types.EventStateTup, 0)
	for tup, evID := range currentStateMap {
		if _, found := resolvedStateMap[tup]; !found {
			toClear = append(toClear, types.EventStateTup{
				StateTup: tup,
				EventID:  evID,
			})
		}
	}
	for tup, evID := range resolvedStateMap {
		if id, found := currentStateMap[tup]; !found || id != evID {
			toSet = append(toSet, types.EventStateTup{
				StateTup: tup,
				EventID:  evID,
			})
		}
	}

	log.Warn().
		Int("to_set", len(toSet)).
		Int("to_clear", len(toClear)).
		Int("existing", len(currentStateMap)).
		Int("resolved", len(resolvedStateMap)).
		Msg("Applying resolved room state")

	for _, tup := range toClear {
		log.Trace().Any("state", tup).Msg("Delete state event references")
		r.txnDeleteStateEvent(txn, eventsProvider.MustGet(tup.EventID))
	}

	var roomChanged bool
	latestVersion := evs[len(evs)-1].IncompleteVersion

	for _, tup := range toSet {
		log.Trace().Any("state", tup).Msg("Store state event references")
		// Increase the version because we want the event in question (restored state) to appear
		// after previous state. This means the historical states of the events persisted in
		// this batch may differ between servers, but should converge on the resolved state.
		// TODO: this is very dangerous if the state reset has >65k changes
		latestVersion.UserVersion += 1
		ev := eventsProvider.MustGet(tup.EventID)
		if r.txnStoreStateEvent(ctx, txn, room, ev, latestVersion, changedUsers, changedServers) {
			roomChanged = true
		}
	}

	if roomChanged {
		txn.Set(r.KeyForRoom(room.ID), room.ToMsgpack())
	}

	// Re-pack the last resolved tup and save
	evIDs := make([]tuple.TupleElement, len(extremEventIDs))
	for i, id := range extremEventIDs {
		evIDs[i] = id.String()
	}
	txn.Set(lastResolvedIDsKey, append(tuple.Tuple{}, evIDs...).Pack())
	txn.SetVersionstampedValue(lastResolvedVersionKey, types.MustVersionstampToBytes(latestVersion))
	return nil
}

// Store events handles writing out all the relevant event data into FoundationDB
// assuming that all events passed in are already authenticated.
func (r *RoomsDatabase) txnStoreEvents(
	ctx context.Context,
	txn fdb.Transaction,
	room *types.Room,
	evs []*types.Event,
	changedUsers map[id.UserID]struct{},
	changedServers map[string]struct{},
) bool {
	if len(evs) > types.MaxVersionstampUserVersion {
		panic("not safe to write this many events in one transaction")
	} else if len(evs) == 0 {
		return false
	}

	zerolog.Ctx(ctx).Debug().Int("events", len(evs)).Msg("Storing batch of events")

	var version tuple.Versionstamp
	depthKey := r.KeyForRoomDepth(room.ID)
	depth := types.BytesToRoomDepth(txn.Get(depthKey).MustGet())

	var eventsStored bool
	var roomChanged bool

	for i, ev := range evs {
		if ev.Outlier {
			panic("cannot pass outliers to txnStoreEvents")
		} else if ev.IsDuplicate {
			continue
		}

		zerolog.Ctx(ctx).Trace().Any("event", ev).Msg("Storing event")

		eventsStored = true
		if ev.Type == event.StateCreate {
			roomChanged = true
		}

		eventTupBytes := types.EventTupToBytes(ev.EventTup())

		// Firstly, store the event itself
		r.events.TxnStoreEvent(txn, ev)

		// This is the magic FDB version which is globally ordered, we index events by this
		version = tuple.IncompleteVersionstamp(uint16(i))
		ev.IncompleteVersion = version // store for later

		// Store global version -> EventTup, this is our index of all events
		txn.SetVersionstampedKey(r.events.KeyForVersion(version), eventTupBytes)

		if ev.Rejected || ev.SoftFailed {
			// We can't just point soft failed events at the new version we're
			// about to create because they aren't valid at that point. They are
			// valid at their prev events, however, so we point their version to
			// the first one of those. This means we resolve the correct historical
			// state at a soft failed event.
			firstPrevVersion := r.events.TxnLookupVersionForEventID(txn, ev.PrevEventIDs[0])
			if firstPrevVersion == types.ZeroVersionstamp {
				// If we don't have a prev event, this event is an outlier, update it
				ev.Outlier = true
				r.events.TxnStoreEvent(txn, ev)
			} else {
				txn.Set(r.events.KeyForIDToVersion(ev.ID), types.MustVersionstampToBytes(firstPrevVersion))
			}
			// Now we've stored the event, global version index and it's own version
			// we're done here, since soft failed events don't appear to clients.
			continue
		}

		if ev.Depth > depth {
			// Update room depth
			txn.Set(depthKey, types.RoomDepthToBytes(ev.Depth))
			depth = ev.Depth
		}

		// Store global event_id -> version
		txn.SetVersionstampedValue(r.events.KeyForIDToVersion(ev.ID), types.MustVersionstampToBytes(version))

		// Room indices
		// room/version -> EventTup, used to sync room events to clients
		txn.SetVersionstampedKey(r.events.KeyForRoomVersion(room.ID, version), eventTupBytes)
		if ev.Local {
			// local events room/version -> EventTup
			txn.SetVersionstampedKey(r.events.KeyForLocalRoomVersion(room.ID, version), eventTupBytes)
		}

		// State events indices
		if ev.StateKey != nil {
			if r.txnStoreStateEvent(ctx, txn, room, ev, version, changedUsers, changedServers) {
				roomChanged = true
			}
		}

		// Relation events indices
		relEvID, relType := ev.RelatesTo()
		if relEvID != "" {
			txn.Set(
				r.events.KeyForRoomRelation(room.ID, relEvID, version),
				tuple.Tuple{ev.ID.String(), []byte(relType)}.Pack(),
			)

			if relType == event.RelThread {
				// room-threads/root-ev-version -> root event ID - only if this
				// doesn't already exist (so the first reply in a thread creates).
				relEvVersion := r.events.TxnLookupVersionForEventID(txn, relEvID)
				threadKey := r.events.KeyForRoomThread(room.ID, relEvVersion)
				if txn.Get(threadKey).MustGet() == nil {
					txn.Set(threadKey, []byte(relEvID))
				}
			}

			if relType == event.RelAnnotation {
				// room-ev-reactions/rel-ev/uid/key
				// Note: dupe check is handled before we call storeEvents
				txn.Set(r.events.KeyForRoomReaction(room.ID, relEvID, ev.Sender, ev.ReactionKey()), []byte(ev.ID))
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
			r.events.TxnDeleteRoomExtremEventID(txn, room.ID, prevEventID)
		}
		// Set this last, so rooms always have a last event
		r.events.TxnSetRoomExtremEventID(txn, room.ID, ev.ID)
	}

	if roomChanged {
		txn.Set(r.KeyForRoom(room.ID), room.ToMsgpack())
	}

	if eventsStored {
		// Bump the room version to the max
		txn.SetVersionstampedValue(r.KeyForRoomVersion(room.ID), types.MustVersionstampToBytes(version))
	}

	return eventsStored
}

// Store references to a state event that will be used to fetch a) current state and b) versioned
// state of room *at* the provided event (includes the event). Note that this method must not store
// anything that cannot later be cleared (ie by a state reset), see `txnDeleteStateEvent`. Returns
// a bool indicating whether the input room object has been changed.
func (r *RoomsDatabase) txnStoreStateEvent(
	ctx context.Context,
	txn fdb.Transaction,
	room *types.Room,
	ev *types.Event,
	version tuple.Versionstamp,
	changedUsers map[id.UserID]struct{},
	changedServers map[string]struct{},
) bool {
	zerolog.Ctx(ctx).Debug().Any("state_tup", ev.StateTup()).Msg("Storing state event")

	// room/version -> EventStateTup
	txn.SetVersionstampedKey(
		r.events.KeyForRoomStateVersion(room.ID, version),
		types.EventStateTupToBytes(ev.EventStateTup()),
	)

	// room/type/state_key/version -> event_id
	txn.SetVersionstampedKey(
		r.events.KeyForRoomStateTupVersion(room.ID, ev.Type, *ev.StateKey, version),
		[]byte(ev.ID),
	)

	if ev.Type == event.StateMember {
		// Process user and server membership changes
		r.txnStoreMembershipEvent(ctx, txn, ev, version, changedUsers, changedServers)
	} else {
		// Current (non member) room/type/state_key -> event_id
		txn.Set(r.events.KeyForRoomCurrentStateTup(room.ID, ev.Type, *ev.StateKey), []byte(ev.ID))
	}

	return r.updateRoomForStateEvent(room, ev)
}

// Member event handling involves a bunch more steps, we need to keep track of which users, and
// servers, are in the room. We also track membership changes by user/server.
func (r *RoomsDatabase) txnStoreMembershipEvent(
	ctx context.Context,
	txn fdb.Transaction,
	ev *types.Event,
	version tuple.Versionstamp,
	changedUsers map[id.UserID]struct{},
	changedServers map[string]struct{},
) {
	memberID := id.UserID(*ev.StateKey)
	membershipTup := ev.MembershipTup()

	zerolog.Ctx(ctx).Debug().
		Any("membership_tup", membershipTup).
		Str("member_id", memberID.String()).
		Msg("Storing membership event")

	changedUsers[memberID] = struct{}{}
	membershipTupBytes := types.MembershipTupToBytes(membershipTup)

	// Current room/member -> MembershipTup
	txn.Set(r.events.KeyForCurrentRoomMember(ev.RoomID, memberID), membershipTupBytes)

	// Current user/room_id -> MembershipTup
	r.users.TxnStoreMembership(txn, memberID, ev.RoomID, membershipTup)

	// User user/member_changes/version -> MembershipTup
	r.users.TxnStoreMembershipChange(txn, memberID, version, membershipTup)

	// Handle server room memberships (including local!)
	username, serverName, _ := memberID.Parse()
	serverJoinedMemberKey := r.servers.KeyForRoomJoinedMember(ev.RoomID, serverName, username)

	// Check if the server is currently joined (servers are only joined or nothing)
	wasServerJoined := r.servers.TxnIsServerJoinedRoom(txn, serverName, ev.RoomID)
	if ev.Membership() == event.MembershipJoin {
		// Set the joined member key and the server membership
		txn.Set(serverJoinedMemberKey, []byte{})
		if !wasServerJoined {
			zerolog.Ctx(ctx).Debug().Str("server", serverName).Msg("Server has joined room")
			changedServers[serverName] = struct{}{}
			r.events.TxnStoreServerMembership(txn, ev.RoomID, serverName, membershipTup, version)
			r.servers.TxnStoreServerMembership(txn, ev.RoomID, serverName, membershipTup, version)
		}
	} else if wasServerJoined {
		txn.Clear(serverJoinedMemberKey)
		// If leaving, now we've cleared the specific member key
		// we check if the server has any other joined members.
		haveOtherJoinedMembers := len(txn.GetRange(
			r.servers.RangeForRoomJoinedMembers(ev.RoomID, serverName),
			fdb.RangeOptions{
				Limit: 1,
			},
		).GetSliceOrPanic()) > 0
		if !haveOtherJoinedMembers {
			zerolog.Ctx(ctx).Debug().Str("server", serverName).Msg("Server has left room")
			changedServers[serverName] = struct{}{}
			// Note any non-join membership is handled here so we  create a new leave MembershipTup
			leaveMtup := types.MembershipTup{
				EventID:    ev.ID,
				RoomID:     ev.RoomID,
				Membership: event.MembershipLeave,
			}
			r.events.TxnStoreServerMembership(txn, ev.RoomID, serverName, leaveMtup, version)
			r.servers.TxnStoreServerMembership(txn, ev.RoomID, serverName, leaveMtup, version)
		}
	}
}

// Removes references to a state event such that it never appears to have existed as part of the
// room state. Removes both room state versions and all user/server memberships and changes.
func (r *RoomsDatabase) txnDeleteStateEvent(txn fdb.Transaction, ev *types.Event) {
	// First get the version *at which the event was written* - if the event is within this batch
	// then we must use the IncompleteVersion field.
	version := ev.IncompleteVersion
	if version == types.ZeroVersionstamp {
		version = r.events.TxnLookupVersionForEventID(txn, ev.ID)
	}

	// Remove room version/local version
	txn.Clear(r.events.KeyForRoomStateVersion(ev.RoomID, version))
	txn.Clear(r.events.KeyForRoomStateTupVersion(ev.RoomID, ev.Type, *ev.StateKey, version))

	if ev.Type == event.StateMember {
		// Clear out user/server memberships and room member state
		memberID := id.UserID(*ev.StateKey)
		_, serverName, _ := memberID.Parse()
		txn.Clear(r.events.KeyForCurrentRoomMember(ev.RoomID, memberID))
		txn.Clear(r.events.KeyForCurrentRoomServer(ev.RoomID, serverName))
		r.users.TxnDeleteUserMembership(txn, memberID, ev.RoomID, version)
		r.servers.TxnDeleteServerMembership(txn, ev.RoomID, serverName, version)
	} else {
		// Clear our room/type/state version
		txn.Clear(r.events.KeyForRoomCurrentStateTup(ev.RoomID, ev.Type, *ev.StateKey))
	}
}
