package events

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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

// Room state extremities
//

// TxnSetRoomStateExtremEventID makes an accepted state event an extremity in place of those it cites
func (e *EventsDirectory) TxnSetRoomStateExtremEventID(
	txn fdb.Transaction,
	roomID id.RoomID,
	eventID id.EventID,
	version tuple.Versionstamp,
	replaced []id.EventID,
) {
	for _, replacedID := range replaced {
		txn.Clear(e.keyForRoomStateExtrem(roomID, replacedID))
	}
	txn.SetVersionstampedValue(e.keyForRoomStateExtrem(roomID, eventID), types.MustVersionstampToBytes(version))
}

// TxnResetRoomStateExtremEventIDs replaces the room's state extremities, as a remote join's publish does
func (e *EventsDirectory) TxnResetRoomStateExtremEventIDs(txn fdb.Transaction, roomID id.RoomID, eventIDs []id.EventID, version tuple.Versionstamp) {
	txn.ClearRange(e.roomStateExtremIDs.Sub(roomID.String()))
	for _, eventID := range eventIDs {
		txn.SetVersionstampedValue(e.keyForRoomStateExtrem(roomID, eventID), types.MustVersionstampToBytes(version))
	}
}

// TxnLookupCurrentRoomStateExtremEventIDs returns the extremities in publish order
func (e *EventsDirectory) TxnLookupCurrentRoomStateExtremEventIDs(txn fdb.ReadTransaction, roomID id.RoomID) []id.EventID {
	iter := txn.GetRange(
		e.roomStateExtremIDs.Sub(roomID.String()),
		fdb.RangeOptions{Mode: fdb.StreamingModeWantAll},
	).Iterator()
	var ids []id.EventID
	var versions []tuple.Versionstamp
	for iter.Advance() {
		kv := iter.MustGet()
		tup, err := e.roomStateExtremIDs.Unpack(kv.Key)
		if err != nil {
			panic(err)
		}
		ids = append(ids, id.EventID(tup[1].(string)))
		versions = append(versions, types.MustBytesToVersionstamp(kv.Value))
	}
	return OrderByVersion(ids, versions)
}

// TxnOrderByStoredVersion returns events in the order they were stored, those not stored first
func (e *EventsDirectory) TxnOrderByStoredVersion(txn fdb.ReadTransaction, eventIDs []id.EventID) []id.EventID {
	futures := make([]fdb.FutureByteSlice, len(eventIDs))
	for i, eventID := range eventIDs {
		futures[i] = txn.Get(e.KeyForIDToVersion(eventID))
	}
	versions := make([]tuple.Versionstamp, len(eventIDs))
	for i, future := range futures {
		if b := future.MustGet(); b != nil {
			versions[i] = types.MustBytesToVersionstamp(b)
		}
	}
	return OrderByVersion(eventIDs, versions)
}

// OrderByVersion returns events ordered by the versions given for them, then by event ID
func OrderByVersion(eventIDs []id.EventID, versions []tuple.Versionstamp) []id.EventID {
	order := make([]int, len(eventIDs))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(bytes.Compare(versions[a].Bytes(), versions[b].Bytes()), cmp.Compare(eventIDs[a], eventIDs[b]))
	})
	ordered := make([]id.EventID, len(order))
	for i, index := range order {
		ordered[i] = eventIDs[index]
	}
	return ordered
}

// Room state DAG
//

func (e *EventsDirectory) keyForRoomStateDAG(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	key, err := e.roomStateDAG.PackWithVersionstamp(tuple.Tuple{roomID.String(), version})
	if err != nil {
		panic(err)
	}
	return key
}

func (e *EventsDirectory) TxnAddRoomStateDAGEvent(txn fdb.Transaction, roomID id.RoomID, eventID id.EventID, version tuple.Versionstamp) {
	txn.SetVersionstampedKey(e.keyForRoomStateDAG(roomID, version), []byte(eventID))
}

// TxnRoomStateDAGPage reads in the order events were added, returning the key to read on from, nil
// past the last
func (e *EventsDirectory) TxnRoomStateDAGPage(txn fdb.ReadTransaction, roomID id.RoomID, after fdb.Key, limit int) ([]id.EventID, fdb.Key, error) {
	begin, end := e.roomStateDAG.Sub(roomID.String()).FDBRangeKeys()
	if after != nil {
		begin = append(slices.Clip(after), 0)
	}
	kvs, err := txn.GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll},
	).GetSliceWithError()
	if err != nil {
		return nil, nil, err
	}
	eventIDs := make([]id.EventID, len(kvs))
	for i, kv := range kvs {
		eventIDs[i] = id.EventID(kv.Value)
	}
	if len(kvs) < limit {
		return eventIDs, nil, nil
	}
	return eventIDs, kvs[len(kvs)-1].Key, nil
}

// TxnRoomStateDAGLonger resolves the key limit+1 rows in on the storage servers rather than reading them
func (e *EventsDirectory) TxnRoomStateDAGLonger(txn fdb.ReadTransaction, roomID id.RoomID, limit int) (bool, error) {
	begin, end := e.roomStateDAG.Sub(roomID.String()).FDBRangeKeys()
	key, err := txn.GetKey(fdb.KeySelector{Key: begin, Offset: limit + 1}).Get()
	if err != nil {
		return false, err
	}
	return bytes.Compare(key, end.FDBKey()) < 0, nil
}
