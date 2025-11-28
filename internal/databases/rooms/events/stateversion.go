package events

import (
	"context"
	"fmt"
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
) (tuple.Versionstamp, error) {
	key := e.KeyForIDToVersion(eventID)
	b, err := txn.Get(key).Get()
	if err != nil {
		return types.ZeroVersionstamp, err
	} else if b == nil {
		return types.ZeroVersionstamp, types.ErrEventNotFound
	}
	return types.BytesToVersionstamp(b)
}

func (e *EventsDirectory) TxnMustLookupVersionForEventID(
	txn fdb.ReadTransaction,
	eventID id.EventID,
) tuple.Versionstamp {
	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		panic(err)
	}
	return version
}

func (e *EventsDirectory) TxnLookupRoomStateAndMemberMapAtVersion(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	version tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	// Fetch all state event tups up to now
	stateTups, err := e.TxnPaginateRoomStateEventTups(txn, roomID, types.PaginationOptions{
		From: types.ZeroVersionstamp,
		To:   version,
		Mode: fdb.StreamingModeWantAll,
	}, eventsProvider)
	if err != nil {
		return nil, err
	}

	// Apply each in order, last state wins
	stateMap := make(types.StateMap, 10)
	for _, tup := range stateTups {
		stateMap[tup.StateTup] = tup.EventID
	}
	return stateMap, nil
}

// Lookup room state event IDs before a given event, optionally passing an events provider to start
// fetching the events. This is expensive since we have to fetch *all* room state events up until
// the event in question. Only exists to satisfy "get state at event" S2S API.
func (e *EventsDirectory) TxnLookupRoomStateAndMemberMapAtEvent(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup event version: %w", err)
	}
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
) (types.StateMap, error) {
	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		return nil, err
	}
	version.UserVersion += 1 // FDB range ends are exclusive, the event should be included

	zerolog.Ctx(ctx).UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("at_or_before_event_id", eventID.String())
	})

	return e.TxnLookupRoomAuthStateMapAtVersion(ctx, txn, roomID, version, eventsProvider)
}

func (e *EventsDirectory) TxnLookupRoomAuthStateMapAtVersion(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	version tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	futs := make(map[event.Type]fdb.RangeResult, len(authStateTypes))
	for _, stateType := range authStateTypes {
		futs[stateType] = txn.GetRange(
			e.RangeForRoomVersionStateTup(roomID, stateType, "", version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	stateMap := make(types.StateMap, len(authStateTypes))
	for _, stateType := range authStateTypes {
		results, err := futs[stateType].GetSliceWithError()
		if err != nil {
			return nil, err
		} else if len(results) > 1 {
			panic("more than one key returned for versioned state request")
		} else if results == nil {
			zerolog.Ctx(ctx).Warn().
				Str("room_id", roomID.String()).
				Str("state_type", stateType.String()).
				Str("versionstamp", version.String()).
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

	return stateMap, nil

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
) (types.StateMap, error) {
	version, err := e.TxnLookupVersionForEventID(txn, eventID)
	if err != nil {
		return nil, err
	}
	version.UserVersion += 1 // FDB range ends are exclusive, the event should be included

	futs := make(map[id.UserID]fdb.RangeResult, len(userIDs))
	for _, userID := range userIDs {
		futs[userID] = txn.GetRange(
			e.RangeForRoomVersionStateTup(roomID, event.StateMember, userID.String(), version),
			fdb.RangeOptions{
				Reverse: true,
				Limit:   1,
			},
		)
	}

	stateMap := make(types.StateMap, len(userIDs))
	for _, userID := range userIDs {
		results, err := futs[userID].GetSliceWithError()
		if err != nil {
			return nil, err
		} else if len(results) > 1 {
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

	return stateMap, nil
}

func (e *EventsDirectory) TxnLookupRoomAuthAndSpecificMemberStateMapAtEvent(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventID id.EventID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	stateMap, err := e.TxnLookupRoomAuthStateMapAtEvent(ctx, txn, roomID, eventID, eventsProvider)
	if err != nil {
		return nil, err
	}
	memberMap, err := e.TxnLookupSpecificRoomMemberStateMapAtEvent(ctx, txn, roomID, userIDs, eventID, eventsProvider)
	if err != nil {
		return nil, err
	}
	maps.Copy(stateMap, memberMap)
	return stateMap, nil
}
