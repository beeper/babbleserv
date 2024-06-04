package events

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Types of state event used for authorization - excluding members
var authStateTypes = []event.Type{
	event.StateCreate,
	event.StateJoinRules,
	event.StatePowerLevels,
	// Third party?
}

func FilterAuthStateMap(state map[event.Type]id.EventID) map[event.Type]id.EventID {
	authState := make(map[event.Type]id.EventID, len(state))
	for _, evType := range authStateTypes {
		if evID, found := state[evType]; found {
			authState[evType] = evID
		}
	}
	return authState
}

// Lookup current last event IDs for a room - note we do not start fetching the
// events as we only need the IDs.
func (e *EventsDirectory) TxnLookupCurrentRoomLastEventIDs(
	txn fdb.Transaction,
	roomID id.RoomID,
) ([]id.EventID, error) {
	prefixIter := txn.GetRange(
		e.RangeForRoomLasts(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make([]id.EventID, 0, 1)
	for prefixIter.Advance() {
		kv, err := prefixIter.Get()
		if err != nil {
			return nil, err
		}
		tup, err := e.ByRoomLast.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id.EventID(tup[1].(string)))
	}
	return ids, nil
}

// Lookup current state (non member) event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentRoomStateEventIDs(
	txn fdb.Transaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (map[event.Type]id.EventID, error) {
	prefixIter := txn.GetRange(
		e.RangeForCurrentRoomState(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make(map[event.Type]id.EventID)
	for prefixIter.Advance() {
		kv, err := prefixIter.Get()
		if err != nil {
			return nil, err
		}
		tup, err := e.ByRoomCurrentState.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		eventsProvider.WillGet(id.EventID(kv.Value)) // FDB start fetch ev
		ids[event.NewEventType(tup[1].(string))] = id.EventID(kv.Value)
	}
	return ids, nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthStateEventIDs(
	txn fdb.Transaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (map[event.Type]id.EventID, error) {
	state, err := e.TxnLookupCurrentRoomStateEventIDs(txn, roomID, eventsProvider)
	if err != nil {
		return nil, err
	}
	return FilterAuthStateMap(state), nil
}

// Lookup current room member event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentRoomMemberEventIDs(
	txn fdb.Transaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventsProvider *TxnEventsProvider,
) (map[id.UserID]id.EventID, error) {
	idToFut := make(map[id.UserID]fdb.FutureByteSlice, len(userIDs))
	for _, uid := range userIDs {
		idToFut[uid] = txn.Get(e.KeyForRoomMember(roomID, uid))
	}
	idToEventID := make(map[id.UserID]id.EventID, len(userIDs))

	for uid, fut := range idToFut {
		b, err := fut.Get()
		if err != nil {
			return nil, err
		} else if b == nil {
			continue
		}
		tup, err := tuple.Unpack(b)
		if err != nil {
			return nil, err
		}
		evID := id.EventID(tup[0].([]byte))
		eventsProvider.WillGet(evID) // FDB start fetch ev
		idToEventID[uid] = evID
	}
	return idToEventID, nil
}

// Lookup versionstamp for a given event
func (e *EventsDirectory) TxnLookupVersionForEventID(
	txn fdb.Transaction,
	eventID id.EventID,
) (tuple.Versionstamp, error) {
	key := e.KeyForIDToVersion(eventID)
	b, err := txn.Get(key).Get()
	if err != nil {
		return tuple.Versionstamp{}, err
	}
	tup, err := tuple.Unpack(b)
	if err != nil {
		return tuple.Versionstamp{}, err
	}

	return tup[0].(tuple.Versionstamp), nil
}

// Return a future to lookup room state at or before an event
func (e *EventsDirectory) TxnLookupRoomStateEventIDsAtEventFut(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	eventID id.EventID,
) func() (map[event.Type]id.EventID, error) {
	log := log.Ctx(ctx).With().
		Str("component", "database").
		Str("transaction", "LookupRoomStateEventIDsAtEventFut").
		Logger()

	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		return func() (map[event.Type]id.EventID, error) { return nil, err }
	}

	futs := make(map[event.Type]fdb.RangeResult, len(authStateTypes))
	for _, stateType := range authStateTypes {
		futs[stateType] = txn.GetRange(
			e.RangeForRoomVersionState(roomID, event.StateCreate, "", version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	return func() (map[event.Type]id.EventID, error) {
		typeToEventID := make(map[event.Type]id.EventID, len(authStateTypes))
		for _, stateType := range authStateTypes {
			results, err := futs[stateType].GetSliceWithError()
			if err != nil {
				return nil, err
			} else if len(results) > 1 {
				panic("more than one key returned for versioned state request")
			} else if results == nil {
				log.Warn().
					Str("room_id", roomID.String()).
					Str("state_type", stateType.String()).
					Str("versionstamp", version.String()).
					Str("at_or_before_event_id", eventID.String()).
					Msg("No historical state event found in room")
				continue
			}
			typeToEventID[stateType] = id.EventID(results[0].Value)
		}
		return typeToEventID, nil
	}
}

func (e *EventsDirectory) TxnLookupRoomStateEventIDsAtEvent(ctx context.Context, txn fdb.Transaction, roomID id.RoomID, eventID id.EventID) (map[event.Type]id.EventID, error) {
	return e.TxnLookupRoomStateEventIDsAtEventFut(ctx, txn, roomID, eventID)()
}

// Return a future to lookup room member state at or before an events
func (e *EventsDirectory) TxnLookupRoomMemberStateEventIDsAtEventFut(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventID id.EventID,
) func() (map[id.UserID]id.EventID, error) {
	log := log.Ctx(ctx).With().
		Str("component", "database").
		Str("transaction", "LookupRoomMemberStateEventIDsAtEventFut").
		Logger()

	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		return func() (map[id.UserID]id.EventID, error) { return nil, err }
	}

	futs := make(map[id.UserID]fdb.RangeResult, len(userIDs))
	for _, userID := range userIDs {
		userIDStr := userID.String()
		futs[userID] = txn.GetRange(
			e.RangeForRoomVersionState(roomID, event.StateMember, userIDStr, version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	return func() (map[id.UserID]id.EventID, error) {
		userIDToEventID := make(map[id.UserID]id.EventID, len(userIDs))
		for _, userID := range userIDs {
			results, err := futs[userID].GetSliceWithError()
			if err != nil {
				return nil, err
			} else if len(results) > 1 {
				panic("more than one key returned for versioned member state request")
			} else if results == nil {
				log.Warn().
					Str("room_id", roomID.String()).
					Str("user_id", userID.String()).
					Str("versionstamp", version.String()).
					Str("at_or_before_event_id", eventID.String()).
					Msg("No historical member state event found in room")
				continue
			}
			userIDToEventID[userID] = id.EventID(results[0].Value)
		}
		return userIDToEventID, nil
	}
}
