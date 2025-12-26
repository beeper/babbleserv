package events

import (
	"context"
	"maps"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Room extremeties
//

func (e *EventsDirectory) TxnDeleteRoomExtremEventID(txn fdb.Transaction, roomID id.RoomID, eventID id.EventID) {
	txn.Clear(e.keyForRoomExtrem(roomID, eventID))
}

func (e *EventsDirectory) TxnSetRoomExtremEventID(txn fdb.Transaction, roomID id.RoomID, eventID id.EventID) {
	txn.Set(e.keyForRoomExtrem(roomID, eventID), []byte{})
}

func (e *EventsDirectory) TxnResetRoomExtremEventIDs(txn fdb.Transaction, roomID id.RoomID, eventID id.EventID) {
	txn.ClearRange(e.rangeForRoomExtrems(roomID))
	e.TxnSetRoomExtremEventID(txn, roomID, eventID)
}

// Lookup current last event IDs for a room - note we do not start fetching the
// events as we only need the IDs.
func (e *EventsDirectory) TxnLookupCurrentRoomExtremEventIDs(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
) []id.EventID {
	iter := txn.GetRange(
		e.rangeForRoomExtrems(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()
	ids := make([]id.EventID, 0, 1)
	for iter.Advance() {
		kv := iter.MustGet()
		ids = append(ids, e.roomExtremKeyToEventID(kv.Key))
	}
	return ids
}

// Current room state (non-member) events
//

func (e *EventsDirectory) TxnIsRoomEncrypted(txn fdb.ReadTransaction, roomID id.RoomID) bool {
	// Note we're simply checking for the presence of an encryption event here
	b := txn.Get(e.KeyForRoomCurrentStateTup(roomID, event.StateEncryption, "")).MustGet()
	return b != nil
}

func (e *EventsDirectory) TxnGetCurrentRoomStateEvent(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	stateTup types.StateTup,
	eventsProvider *TxnEventsProvider,
) *types.Event {
	var sMap types.StateMap

	// Member state events are special and are stored separately to other state events
	if stateTup.Type == event.StateMember {
		sMap = e.TxnLookupCurrentSpecificRoomMemberStateMap(
			txn,
			roomID,
			[]id.UserID{id.UserID(stateTup.StateKey)},
			eventsProvider,
		)
	} else {
		sMap = e.TxnLookupCurrentSpecificRoomStateTupMap(
			txn,
			roomID,
			[]types.StateTup{stateTup},
			eventsProvider,
		)
	}

	if len(sMap) == 0 {
		return nil
	}

	eventID := sMap[stateTup]
	return eventsProvider.MustGet(eventID)
}

func (e *EventsDirectory) TxnFilterJoinedMembershipsWithEncryption(
	txn fdb.ReadTransaction,
	memberships types.Memberships,
) (types.Memberships, error) {
	// Find joins and kick off fetches for the room encryption event state tup
	futs := make(map[id.RoomID]fdb.FutureByteSlice, len(memberships))
	for roomID, membershipTup := range memberships {
		if membershipTup.Membership == event.MembershipJoin {
			futs[roomID] = txn.Get(e.KeyForRoomCurrentStateTup(roomID, event.StateEncryption, ""))
		}
	}

	// Now make new memberships for only rooms with an encryption event
	newMemberships := make(types.Memberships, len(futs))
	for roomID, fut := range futs {
		b, err := fut.Get()
		if err != nil {
			return nil, err
		} else if b != nil {
			newMemberships[roomID] = memberships[roomID]
		}
	}

	return newMemberships, nil
}

// Lookup specific state events
func (e *EventsDirectory) TxnLookupCurrentSpecificRoomStateTupMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	tups []types.StateTup,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	tupToFut := make(map[types.StateTup]fdb.FutureByteSlice, len(tups))
	for _, tup := range tups {
		tupToFut[tup] = txn.Get(e.KeyForRoomCurrentStateTup(roomID, tup.Type, tup.StateKey))
	}

	stateMap := make(types.StateMap, len(tups))
	for tup, fut := range tupToFut {
		if eid := fut.MustGet(); eid != nil {
			stateMap[tup] = id.EventID(eid)
			if eventsProvider != nil {
				eventsProvider.WillGet(id.EventID(eid))
			}
		}
	}

	return stateMap
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	return e.TxnLookupCurrentSpecificRoomStateTupMap(txn, roomID, authStateTups, eventsProvider)
}

func (e *EventsDirectory) TxnLookupCurrentRoomStrippedStateStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	return e.TxnLookupCurrentSpecificRoomStateTupMap(txn, roomID, strippedStateTups, eventsProvider)
}

// Lookup current state (non member) event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentRoomStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	iter := txn.GetRange(
		e.RangeForRoomCurrentState(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make(types.StateMap)
	for iter.Advance() {
		kv := iter.MustGet()
		stateTup := e.CurrentRoomStateKeyValueToStateTup(kv)
		ids[stateTup.StateTup] = id.EventID(kv.Value)
		if eventsProvider != nil {
			eventsProvider.WillGet(stateTup.EventID)
		}
	}
	return ids
}

// Current room memberships (users & servers)
//

func (e *EventsDirectory) TxnLookupCurrentRoomMemberships(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.RoomMemberships {
	kvs := txn.GetRange(
		e.RangeForCurrentRoomMembers(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).GetSliceOrPanic()

	ids := make(types.RoomMemberships, len(kvs))
	for _, kv := range kvs {
		stateTup, membershipTup := e.CurrentRoomMemberKeyValueToTups(kv)
		if eventsProvider != nil {
			eventsProvider.WillGet(membershipTup.EventID)
		}
		ids[id.UserID(stateTup.StateKey)] = membershipTup
	}

	return ids
}

func (e *EventsDirectory) TxnLookupCurrentRoomMemberStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	iter := txn.GetRange(
		e.RangeForCurrentRoomMembers(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	ids := make(types.StateMap)
	for iter.Advance() {
		kv := iter.MustGet()
		stateTup, membershipTup := e.CurrentRoomMemberKeyValueToTups(kv)
		if eventsProvider != nil {
			eventsProvider.WillGet(membershipTup.EventID)
		}
		ids[stateTup.StateTup] = membershipTup.EventID
	}

	return ids
}

func (e *EventsDirectory) TxnLookupCurrentRoomStateAndMemberMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	stateMap := e.TxnLookupCurrentRoomStateMap(txn, roomID, eventsProvider)
	memberMap := e.TxnLookupCurrentRoomMemberStateMap(txn, roomID, eventsProvider)
	maps.Copy(stateMap, memberMap)
	return stateMap
}

func (e *EventsDirectory) TxnLookupCurrentRoomServers(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
) ([]string, error) {
	iter := txn.GetRange(
		e.rangeForCurrentRoomServers(roomID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	serverNames := make([]string, 0)
	for iter.Advance() {
		kv := iter.MustGet()
		serverNames = append(serverNames, e.currentRoomServerKeyToServer(kv.Key))
	}

	return serverNames, nil
}

// Lookup current room member event IDs and start fetching events
func (e *EventsDirectory) TxnLookupCurrentSpecificRoomMemberStateMap(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	idToFut := make(map[id.UserID]fdb.FutureByteSlice, len(userIDs))
	for _, uid := range userIDs {
		idToFut[uid] = txn.Get(e.KeyForCurrentRoomMember(roomID, uid))
	}
	idToEventID := make(types.StateMap, len(userIDs))

	for uid, fut := range idToFut {
		b := fut.MustGet()
		if b == nil {
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
	return idToEventID
}

func (e *EventsDirectory) TxnLookupCurrentRoomAuthAndSpecificMemberStateMap(
	ctx context.Context,
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	stateMap := e.TxnLookupCurrentRoomAuthStateMap(txn, roomID, eventsProvider)
	memberMap := e.TxnLookupCurrentSpecificRoomMemberStateMap(txn, roomID, userIDs, eventsProvider)
	maps.Copy(stateMap, memberMap)
	return stateMap
}
