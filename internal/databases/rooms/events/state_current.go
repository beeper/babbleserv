package events

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Lookup current last event IDs for a room - note we do not start fetching the
// events as we only need the IDs.
func (e *EventsDirectory) TxnLookupCurrentRoomExtremEventIDs(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
) ([]id.EventID, error) {
	iter := txn.GetRange(
		e.RangeForRoomExtrems(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make([]id.EventID, 0, 1)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		ids = append(ids, e.RoomExtremKeyToEventID(kv.Key))
	}
	return ids, nil
}

// Lookup current state (non member) event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentRoomStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	iter := txn.GetRange(
		e.RangeForRoomCurrentState(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make(types.StateMap)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		stateTup := e.CurrentRoomStateKeyValueToStateTup(kv)
		ids[stateTup.StateTup] = id.EventID(kv.Value)
		if eventsProvider != nil {
			eventsProvider.WillGet(stateTup.EventID)
		}
	}
	return ids, nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	state, err := e.TxnLookupCurrentRoomStateMap(txn, roomID, nil)
	if err != nil {
		return nil, err
	}
	return filterStateMap(state, authStateTypes, eventsProvider), nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomInviteStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	state, err := e.TxnLookupCurrentRoomStateMap(txn, roomID, nil)
	if err != nil {
		return nil, err
	}
	return filterStateMap(state, inviteStateTypes, eventsProvider), nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomMemberStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	iter := txn.GetRange(
		e.RangeForCurrentRoomMembers(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make(types.StateMap)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		stateTup, membershipTup := e.CurrentRoomMemberKeyValueToTups(kv)
		if eventsProvider != nil {
			eventsProvider.WillGet(membershipTup.EventID)
		}
		ids[stateTup.StateTup] = membershipTup.EventID
	}

	return ids, nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomServers(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
) ([]string, error) {
	iter := txn.GetRange(
		e.RangeForCurrentRoomServers(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	serverNames := make([]string, 0)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		serverNames = append(serverNames, e.CurrentRoomServerKeyToServer(kv.Key))
	}

	return serverNames, nil
}

// Lookup current room member event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentSpecificRoomMemberStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	idToFut := make(map[id.UserID]fdb.FutureByteSlice, len(userIDs))
	for _, uid := range userIDs {
		idToFut[uid] = txn.Get(e.KeyForCurrentRoomMember(roomID, uid))
	}
	idToEventID := make(types.StateMap, len(userIDs))

	for uid, fut := range idToFut {
		b, err := fut.Get()
		if err != nil {
			return nil, err
		} else if b == nil {
			continue
		}
		membershipTup := types.ValueToMembershipTup(b)
		if eventsProvider != nil {
			eventsProvider.WillGet(membershipTup.EventID)
		}
		idToEventID[types.StateTup{
			Type:     event.StateMember,
			StateKey: uid.String(),
		}] = membershipTup.EventID
	}
	return idToEventID, nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	stateMap, err := e.TxnLookupCurrentRoomAuthStateMap(txn, roomID, eventsProvider)
	if err != nil {
		return nil, err
	}
	memberMap, err := e.TxnLookupCurrentSpecificRoomMemberStateMap(txn, roomID, userIDs, eventsProvider)
	if err != nil {
		return nil, err
	}
	for k, v := range memberMap {
		stateMap[k] = v
	}
	return stateMap, nil
}
