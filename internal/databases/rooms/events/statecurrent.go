package events

import (
	"context"
	"maps"

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

// Lookup specific state events
func (e *EventsDirectory) TxnLookupCurrentStateEventIDs(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	tups []types.StateTup,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	tupToFut := make(map[types.StateTup]fdb.FutureByteSlice, len(tups))
	for _, tup := range tups {
		tupToFut[tup] = txn.Get(e.KeyForRoomCurrentStateTup(roomID, tup.Type, tup.StateKey))
	}

	stateMap := make(types.StateMap, len(tups))
	for tup, fut := range tupToFut {
		eid, err := fut.Get()
		if err != nil {
			return nil, err
		} else if eid != nil {
			stateMap[tup] = id.EventID(eid)
			if eventsProvider != nil {
				eventsProvider.WillGet(id.EventID(eid))
			}
		}
	}

	return stateMap, nil
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	return e.TxnLookupCurrentStateEventIDs(txn, roomID, authStateTups, eventsProvider)
}

func (e *EventsDirectory) TxnLookupCurrentRoomStrippedStateStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	return e.TxnLookupCurrentStateEventIDs(txn, roomID, strippedStateTups, eventsProvider)
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

func (e *EventsDirectory) TxnLookupCurrentRoomStateAndMemberMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) (types.StateMap, error) {
	stateMap, err := e.TxnLookupCurrentRoomStateMap(txn, roomID, eventsProvider)
	if err != nil {
		return nil, err
	}
	memberMap, err := e.TxnLookupCurrentRoomMemberStateMap(txn, roomID, eventsProvider)
	if err != nil {
		return nil, err
	}
	maps.Copy(stateMap, memberMap)
	return stateMap, nil
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
		membershipTup := types.BytesToMembershipTup(b)
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
