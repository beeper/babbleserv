package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"
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
