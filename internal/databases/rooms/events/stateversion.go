package events

import (
	"context"
	"maps"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Lookup versionstamp for a given event
func (e *EventsDirectory) TxnLookupVersionForEventID(
	txn fdb.ReadTransaction,
	eventID id.EventID,
) tuple.Versionstamp {
	key := e.KeyForIDToVersion(eventID)
	b := txn.Get(key).MustGet()
	if b == nil {
		return types.ZeroVersionstamp
	}
	return types.MustBytesToVersionstamp(b)
}

func (e *EventsDirectory) TxnLookupRoomStateAndMemberMapAtVersion(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	version tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	// Fetch all state event tups up to now
	stateTups := e.TxnPaginateRoomStateEventTups(txn, roomID, types.PaginationOptions{
		From: types.ZeroVersionstamp,
		To:   version,
		Mode: fdb.StreamingModeWantAll,
	}, eventsProvider)

	// Apply each in order, last state wins
	stateMap := make(types.StateMap, 10)
	for _, tup := range stateTups {
		stateMap[tup.StateTup] = tup.EventID
	}
	return stateMap
}

// Lookup room state event IDs before a given event, optionally passing an events provider to start
// fetching the events. This is expensive since we have to fetch *all* room state events up until
// the event in question. Only exists to satisfy "get state at event" S2S API.
func (e *EventsDirectory) TxnLookupRoomStateAndMemberMapAtEvent(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	version := e.TxnLookupVersionForEventID(txn, eventID)
	return e.TxnLookupRoomStateAndMemberMapAtVersion(txn, roomID, version, eventsProvider)
}

// Lookup room auth state at or before an event, that is the most recent power_levels, join_rules,
// etc events that occurred before the version of the input event.
func (e *EventsDirectory) TxnLookupRoomAuthStateMapAtEvent(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	version := e.TxnLookupVersionForEventID(txn, eventID)
	version.UserVersion += 1 // FDB range ends are exclusive, the event should be included

	log := zerolog.Ctx(ctx).With().Str("at_or_before_event_id", eventID.String()).Logger()
	ctx = log.WithContext(ctx)

	return e.TxnLookupRoomAuthStateMapAtVersion(ctx, txn, roomID, version, eventsProvider)
}

func (e *EventsDirectory) TxnLookupRoomAuthStateMapAtVersion(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	version tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	futs := make(map[event.Type]fdb.RangeResult, len(authStateTypes))
	for _, stateType := range authStateTypes {
		futs[stateType] = txn.GetRange(
			e.rangeForRoomVersionStateTup(roomID, stateType, "", version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	stateMap := make(types.StateMap, len(authStateTypes))
	for _, stateType := range authStateTypes {
		results := futs[stateType].GetSliceOrPanic()
		if len(results) > 1 {
			panic("more than one key returned for versioned state request")
		} else if results == nil {
			zerolog.Ctx(ctx).Warn().
				Stringer("room_id", roomID).
				Stringer("state_type", stateType).
				Any("versionstamp", version).
				Msg("No historical state event found in room")
			continue
		}
		stateMap[types.StateTup{
			Type: stateType,
		}] = id.EventID(results[0].Value)
	}

	if eventsProvider != nil {
		for _, evID := range stateMap {
			eventsProvider.WillGet(evID)
		}
	}

	return stateMap

}

// Lookup specific member state at or before an event, that is the most recent room_member event
// that occurred before the version of the input event.
func (e *EventsDirectory) TxnLookupSpecificRoomMemberStateMapAtEvent(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	version := e.TxnLookupVersionForEventID(txn, eventID)
	version.UserVersion += 1 // FDB range ends are exclusive, the event should be included

	futs := make(map[id.UserID]fdb.RangeResult, len(userIDs))
	for _, userID := range userIDs {
		futs[userID] = txn.GetRange(
			e.rangeForRoomVersionStateTup(roomID, event.StateMember, userID.String(), version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	stateMap := make(types.StateMap, len(userIDs))
	for _, userID := range userIDs {
		results := futs[userID].GetSliceOrPanic()
		if len(results) > 1 {
			panic("more than one key returned for versioned member state request")
		} else if results == nil {
			zerolog.Ctx(ctx).Warn().
				Str("room_id", roomID.String()).
				Str("user_id", userID.String()).
				Str("versionstamp", version.String()).
				Str("at_or_before_event_id", eventID.String()).
				Msg("No historical member state event found in room")
			continue
		}
		stateMap[types.StateTup{
			Type:     event.StateMember,
			StateKey: userID.String(),
		}] = id.EventID(results[0].Value)
	}

	if eventsProvider != nil {
		for _, evID := range stateMap {
			eventsProvider.WillGet(evID)
		}
	}

	return stateMap
}

func (e *EventsDirectory) TxnLookupRoomAuthAndSpecificMemberStateMapAtEvent(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	stateMap := e.TxnLookupRoomAuthStateMapAtEvent(ctx, txn, roomID, eventID, eventsProvider)
	memberMap := e.TxnLookupSpecificRoomMemberStateMapAtEvent(ctx, txn, roomID, userIDs, eventID, eventsProvider)
	maps.Copy(stateMap, memberMap)
	return stateMap
}
