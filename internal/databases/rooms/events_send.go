// The send events transactions are the engine of Babbleserv, this contains the
// logic to authorize and ingest events from local homeserver users and events
// coming from other homeservers via federation.

package rooms

import (
	"context"
	"errors"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/log"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type sendEventsResult struct {
	eventMap          map[id.EventID]error
	versionstampFut   fdb.FutureKey
	allowed, rejected int
}

func newResults(allowedEvs []*types.Event, rejectedEvs map[id.EventID]error) sendEventsResult {
	var results sendEventsResult

	eventMap := make(map[id.EventID]error, len(allowedEvs)+len(rejectedEvs))
	for eventID, err := range rejectedEvs {
		eventMap[eventID] = err
		results.rejected += 1
	}
	for _, ev := range allowedEvs {
		eventMap[ev.ID] = nil
		results.allowed += 1
	}
	results.eventMap = eventMap
	return results
}

func (r *sendEventsResult) EventMap() map[id.EventID]error {
	return r.eventMap
}

func getUserIDList(evs []*types.Event) []id.UserID {
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

// Send local events to a room, populating prev/auth events as well as authorizing
// the new events against the current room state.
func (r *RoomsDatabase) SendLocalEvents(ctx context.Context, roomID id.RoomID, evs []*types.Event) (sendEventsResult, error) {
	log := r.getTxnLogContext(ctx, "SendLocalEvents").
		Str("room_id", roomID.String()).
		Int("events_in", len(evs)).
		Logger()

	ctx = log.WithContext(ctx)

	if result, err := r.db.Transact(func(txn fdb.Transaction) (any, error) {
		txn.Get(r.KeyForRoom(roomID)) // start fetching the room for later

		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn, evs)

		// Get the current room state which we'll use to authenticate the events
		authStateEventIDs, err := r.events.TxnLookupCurrentRoomAuthStateEventIDs(txn, roomID, eventsProvider)
		if err != nil {
			return nil, err
		}

		// Get the current memberships of senders + membership ev state keys
		memberEventIDs, err := r.events.TxnLookupCurrentRoomMemberEventIDs(txn, roomID, getUserIDList(evs), eventsProvider)
		if err != nil {
			return nil, err
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

		prevEventIDs, err := r.events.TxnLookupCurrentRoomLastEventIDs(txn, roomID)
		if err != nil {
			return nil, err
		}

		authProvider := events.NewTxnAuthEventsProvider(
			ctx,
			eventsProvider,
			memberEventIDs,
			authStateEventIDs,
		)

		allowedEvs := make([]*types.Event, 0, len(evs))
		rejectedEvs := make(map[id.EventID]error)

		for _, ev := range evs {
			ev.AuthEventIDs = authProvider.GetAuthEventIDsForEvent(ev)
			ev.PrevEventIDs = prevEventIDs
			if err := authProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Stringer("event_id", ev.ID).
					Any("event", ev).
					Msg("Failed to auth event against current state")
				rejectedEvs[ev.ID] = err
				eventsProvider.Reject(ev.ID)
				continue
			}
			// Set next event to use this one as prev
			prevEventIDs = []id.EventID{ev.ID}
			allowedEvs = append(allowedEvs, ev)
		}

		r.txnStoreEvents(ctx, txn, roomID, allowedEvs)

		results := newResults(allowedEvs, rejectedEvs)
		results.versionstampFut = txn.GetVersionstamp()
		return results, nil
	}); err != nil {
		return sendEventsResult{}, err
	} else {
		res := result.(sendEventsResult)
		log := log.Info().
			Int("events_allowed", res.allowed).
			Int("events_rejected", res.rejected)
		if res.allowed > 0 {
			log = log.Str("versionstamp", res.versionstampFut.MustGet().String())
		}
		log.Msg("Sent local events")
		return res, nil
	}
}

// Send federated events to a room after passing through all the required
// authorization checks. (steps 4-6: https://spec.matrix.org/v1.10/server-server-api/#checks-performed-on-receipt-of-a-pdu)
//
// This method assumes all remote fetching of events has been completed and are
// included in evs, any that are not will be rejected. Since this entire batch
// must be executed in a single FDB txn we only have 5s, so can't waste time
// fetching events.
func (r *RoomsDatabase) SendFederatedEvents(ctx context.Context, roomID id.RoomID, evs []*types.Event) (sendEventsResult, error) {
	log := r.getTxnLogContext(ctx, "SendFederatedEvents").
		Str("room_id", roomID.String()).
		Int("events_in", len(evs)).
		Logger()

	ctx = log.WithContext(ctx)

	if result, err := r.db.Transact(func(txn fdb.Transaction) (any, error) {
		roomKey := r.KeyForRoom(roomID)
		txn.Get(roomKey) // start fetching the room for later

		// First part of the transaction - let's start pulling all the data that
		// we'll need later to authorize events. Doing this here means FDB will
		// fetch the data in the background while we process, critical to keeping
		// below the 5s limit.
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn, evs)

		// Quickly initiate fetches of all the events direct auth+prev events
		// Step 4: Passes authorization rules based on the event’s auth events, otherwise it is rejected.
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

		// Now for each event fetch the auth state for each prev event *at the
		// point in time the prev event was committed*.
		// Step 5: Passes authorization rules based on the state before the event, otherwise it is rejected.
		prevEventIDToStateFut := make(map[id.EventID]func() (map[event.Type]id.EventID, error), len(evs))
		prevEventIDToMembersFut := make(map[id.EventID]func() (map[id.UserID]id.EventID, error), len(evs))
		for _, ev := range evs {
			for _, prevID := range ev.PrevEventIDs {
				// Grab the prev event, will block if not loaded yet
				prevEv, err := eventsProvider.Get(prevID)
				if err != nil {
					return nil, err
				}
				// And the prev events auth events
				for _, pEvID := range prevEv.AuthEventIDs {
					eventsProvider.WillGet(pEvID)
				}
				// And the auth state of the room at the time of the prev event
				if _, found := prevEventIDToStateFut[prevID]; !found {
					prevEventIDToStateFut[prevID] = r.events.TxnLookupRoomStateEventIDsAtEventFut(ctx, txn, roomID, prevID)
					prevEventIDToMembersFut[prevID] = r.events.TxnLookupRoomMemberStateEventIDsAtEventFut(ctx, txn, roomID, getUserIDList([]*types.Event{prevEv}), prevID)
				}
			}
		}

		// Get the current room auth state + members
		// Step 6: Passes authorization rules based on the current state of the room, otherwise it is “soft failed”.
		currentAuthStateEventIDs, err := r.events.TxnLookupCurrentRoomAuthStateEventIDs(txn, roomID, eventsProvider)
		if err != nil {
			return nil, err
		}
		currentMemberEventIDs, err := r.events.TxnLookupCurrentRoomMemberEventIDs(txn, roomID, getUserIDList(evs), eventsProvider)
		if err != nil {
			return nil, err
		}

		// Before we auth events, let's quickly check the room itself can federate
		roomBytes := txn.Get(roomKey).MustGet()
		// If roomBytes is nil we're creating the room
		if roomBytes != nil {
			room := types.MustNewRoomFromBytes(roomBytes)
			if !room.Federated {
				return nil, errors.New("this room is not federated")
			}
		}

		// We've now started all the data fetching in the background, now we
		// can actually process each event. Unlike local events, which can rely
		// on the current room state, federated events need to be handled out of
		// order. Before we can store an event we need to authorize it.

		// Use this auth provider throughout as we accept events after step 6
		currentAuthProvider := events.NewTxnAuthEventsProvider(
			ctx,
			eventsProvider,
			currentMemberEventIDs,
			currentAuthStateEventIDs,
		)

		allowedEvs := make([]*types.Event, 0, len(evs))
		rejectedEvs := make(map[id.EventID]error)

		for _, ev := range evs {
			// Step 4: Passes authorization rules based on the event’s auth events, otherwise it is rejected.

			stateMap := make(map[event.Type]id.EventID)
			memberMap := make(map[id.UserID]id.EventID)
			for _, authID := range ev.AuthEventIDs {
				authEv := eventsProvider.MustGet(authID)
				// TODO: reject, move this out of txn
				if authEv.StateKey == nil {
					log.Warn().
						Any("event", authEv).
						Msg("Ignoring non-state auth event")
					continue
				}
				if authEv.StateKeyStr == "" {
					// TODO: any duplicates in auth events should be rejected
					// ideally before this transaction though...
					stateMap[authEv.Type] = authEv.ID
				} else if authEv.Type == event.StateMember {
					memberMap[id.UserID(authEv.Sender)] = authEv.ID
				}
			}

			authProvider := events.NewTxnAuthEventsProvider(
				ctx,
				eventsProvider,
				memberMap,
				stateMap,
			)
			if err := authProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Stringer("event_id", ev.ID).
					Any("event", ev).
					Msg("Failed to auth event (step 4)")
				rejectedEvs[ev.ID] = err
				eventsProvider.Reject(ev.ID)
				continue
			}

			// Step 5: Passes authorization rules based on the state before the event, otherwise it is rejected.
			clear(stateMap)
			clear(memberMap)

			if len(ev.PrevEventIDs) == 1 {
				var err error
				// Shortcut: we only have one prev event so there's only one state
				stateMap, err = prevEventIDToStateFut[ev.PrevEventIDs[0]]()
				if err != nil {
					return nil, err
				}
				memberMap, err = prevEventIDToMembersFut[ev.PrevEventIDs[0]]()
				if err != nil {
					return nil, err
				}
			} else {
				// We have multiple states to resolve, we need all the state events
				// (both conflicted + unconflicted).
				allStateEvents := make([]gomatrixserverlib.PDU, 0)
				// Plus all of those state events auth events.
				allAuthEventIDs := make(map[id.EventID]struct{})

				for _, prevID := range ev.PrevEventIDs {
					// Get all the state events from each prev event
					prevStateMap, err := prevEventIDToStateFut[prevID]()
					if err != nil {
						return nil, err
					}
					for _, stateID := range prevStateMap {
						stateEv := eventsProvider.MustGet(stateID)
						allStateEvents = append(allStateEvents, stateEv.PDU())
						for _, authID := range stateEv.AuthEventIDs {
							allAuthEventIDs[authID] = struct{}{}
						}
					}
					// Get all member events from each prev event
					prevMemberMap, err := prevEventIDToMembersFut[prevID]()
					if err != nil {
						return nil, err
					}
					for _, memberID := range prevMemberMap {
						memberEv := eventsProvider.MustGet(memberID)
						allStateEvents = append(allStateEvents, memberEv.PDU())
						for _, authID := range memberEv.AuthEventIDs {
							allAuthEventIDs[authID] = struct{}{}
						}
					}
				}

				// Finally, grab all the auth events for all the state events we
				// need to resolve.
				allAuthEvents := make([]gomatrixserverlib.PDU, 0)
				for authEvID := range allAuthEventIDs {
					allAuthEvents = append(allAuthEvents, eventsProvider.MustGet(authEvID).PDU())
				}

				resolvedPDUs, err := gomatrixserverlib.ResolveConflicts(
					"11",
					allStateEvents,
					allAuthEvents,
					func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
						return senderID.ToUserID(), nil
					},
					nil,
				)
				if err != nil {
					return nil, err
				}

				// Turn the resolved state back into our stateMap/memberMap
				for _, pdu := range resolvedPDUs {
					ev = pdu.(types.EventPDU).Event()
					if ev.Type == event.StateMember {
						memberMap[id.UserID(*ev.StateKey)] = ev.ID
					} else {
						stateMap[ev.Type] = ev.ID
					}
				}
			}

			authProvider = events.NewTxnAuthEventsProvider(
				ctx,
				eventsProvider,
				memberMap,
				stateMap,
			)
			if err := authProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Stringer("event_id", ev.ID).
					Any("event", ev).
					Msg("Failed to auth event (step 5)")
				rejectedEvs[ev.ID] = err
				eventsProvider.Reject(ev.ID)
				continue
			}

			// Step 6: Passes authorization rules based on the current state of the room, otherwise it is “soft failed”.
			// We're using the current auth provider here which is shared for each
			// event we process. This means any state events in the batch are
			// considered for remaining events.
			if err := currentAuthProvider.IsEventAllowed(ev); err != nil {
				log.Err(err).
					Stringer("event_id", ev.ID).
					Any("event", ev).
					Msg("Soft failed to auth event (step 6)")

				// Flag the event as soft failed to exclude it from room indices
				// and current state. Otherwise should be treated as normal, so
				// from a federation perspective, it succeeded.
				ev.SoftFailed = true
			}

			allowedEvs = append(allowedEvs, ev)
		}

		r.txnStoreEvents(ctx, txn, roomID, allowedEvs)

		results := newResults(allowedEvs, rejectedEvs)
		results.versionstampFut = txn.GetVersionstamp()
		return results, nil
	}); err != nil {
		return sendEventsResult{}, err
	} else {
		res := result.(sendEventsResult)
		log.Info().
			Int("events_allowed", res.allowed).
			Int("events_rejected", res.rejected).
			Str("versionstamp", res.versionstampFut.MustGet().String()).
			Msg("Sent federated events")
		return res, nil
	}
}

// Store events handles writing out all the relevant event data into FoundationDB
// assuming that all events passed in are already authenticated.
func (r *RoomsDatabase) txnStoreEvents(ctx context.Context, txn fdb.Transaction, roomID id.RoomID, evs []*types.Event) {
	// Grab the room object, which may be nil if not created yet
	roomKey := r.KeyForRoom(roomID)
	roomBytes := txn.Get(roomKey).MustGet()
	var room *types.Room
	if roomBytes == nil {
		room = &types.Room{}
	} else {
		room = types.MustNewRoomFromBytes(roomBytes)
	}

	roomChanged := false

	for i, ev := range evs {
		if ev.RoomID != roomID {
			panic("event room ID does not match input room ID")
		}

		log.Ctx(ctx).Trace().
			Str("event_id", ev.ID.String()).
			Str("type", ev.Type.String()).
			Msg("Storing event")

		eventID := []byte(ev.ID)

		// Firstly, store the event itself
		txn.Set(r.events.KeyForEvent(ev.ID), ev.ToMsgpack())

		// This is the magic FDB version which is globally ordered, we index
		// events by this.
		version := tuple.IncompleteVersionstamp(uint16(i))

		// Store version -> event_id
		txn.SetVersionstampedKey(r.events.KeyForVersion(version), eventID)
		// And event_id -> version
		txn.SetVersionstampedValue(r.events.KeyForIDToVersion(ev.ID), util.MakeVersionstampValue(version))

		// Room indices
		// room/version -> event_id, used to sync room events to clients, so we
		// don't add soft failed events here.
		if !ev.SoftFailed {
			txn.SetVersionstampedKey(r.events.KeyForRoomVersion(ev.RoomID, version), eventID)
		}

		if ev.StateKey != nil && !ev.SoftFailed {
			// room/type/state_key/version -> event_id
			txn.SetVersionstampedKey(r.events.KeyForRoomVersionState(ev.RoomID, ev.Type, *ev.StateKey, version), eventID)

			if ev.Type == event.StateMember {
				memberID := id.UserID(*ev.StateKey)
				membership := []byte(ev.Membership())

				// Current room/member -> (event_id, membership)
				txn.Set(r.events.KeyForRoomMember(ev.RoomID, memberID), tuple.Tuple{eventID, membership}.Pack())

				// Current user/room_id -> membership
				txn.Set(r.users.KeyForUserMembership(memberID, ev.RoomID), membership)

				// User user/member_changes/version -> (roomID, membership)
				txn.Set(r.users.KeyForUserMembershipVersion(memberID, version), tuple.Tuple{ev.RoomID.String(), membership}.Pack())
			} else {
				// Current (non member) room/type/state_key -> event_id
				txn.Set(r.events.KeyForRoomCurrentState(ev.RoomID, ev.Type, ev.StateKey), eventID)
			}

			// Now we need to update our room object
			changed := r.updateRoomForStateEvent(roomID, room, ev)
			if changed {
				roomChanged = true
			}
		}

		// Update room last events
		// This is where we handle the partial DAG ordering via prev_events
		// For each new event:
		//     store room/last/event_id empty key
		//     for each ev.prev_events, clear room/last/prev_ev_id
		// The contents of room/last/ are used at event creation time to populate
		// prev_events, thus any DAG split can be corrected by sending an event.
		if !ev.SoftFailed {
			for _, prevEventID := range ev.PrevEventIDs {
				txn.Clear(r.events.KeyForRoomLast(ev.RoomID, prevEventID))
			}
			// Set this last, so rooms always have a last event
			txn.Set(r.events.KeyForRoomLast(ev.RoomID, ev.ID), nil)
		}
	}

	if roomChanged {
		txn.Set(roomKey, room.ToMsgpack())
	}
}

func (r *RoomsDatabase) updateRoomForStateEvent(roomID id.RoomID, room *types.Room, ev *types.Event) bool {
	if room.ID == "" {
		if ev.Type != event.StateCreate {
			panic("no room create event")
		}
		room.ID = roomID

		// Set federated flag from create event content
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

	changed := false

	if ev.Type == event.StateMember {
		membership := ev.Membership()
		switch membership {
		case event.MembershipJoin:
			room.MemberCount += 1
			changed = true
		case event.MembershipLeave, event.MembershipBan:
			// TODO: check previous state?
			room.MemberCount -= 1
			changed = true
		}
	}

	// TODO: canonical alias check
	// ensure we don't conflict with another, set if not set

	// TODO: update room .Name, .Type, .Topic, .AvatarURL

	return changed
}
