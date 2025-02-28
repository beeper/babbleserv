package rooms

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/types"
)

type SuperStreamType string

const (
	SuperStreamEvent   SuperStreamType = "ev"
	SuperStreamReceipt SuperStreamType = "rc"
)

// Rooms super stream items represent either an event or a receipt in the room along with the
// version at which they occurred.
type SuperStreamItem struct {
	Type    SuperStreamType
	Version tuple.Versionstamp

	EventIDTup *types.EventIDTup
	Receipt    *types.Receipt
}

var _ types.Versioner = (*SuperStreamItem)(nil)

func (i SuperStreamItem) GetVersion() tuple.Versionstamp {
	return i.Version
}

func (r *RoomsDatabase) keyForSuperStreamReceiptVersion(rc *types.Receipt) fdb.Key {
	return r.superStreamReceiptVersions.Pack(tuple.Tuple{
		rc.RoomID.String(),
		rc.UserID.String(),
		string(rc.Type),
		rc.ThreadID,
	})
}

// Main rooms super stream
//

func (r *RoomsDatabase) rangeForSuperStream(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.superStream, fromVersion, toVersion, roomID.String())
}

func (r *RoomsDatabase) keyForSuperStreamRoom(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := r.superStream.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (r *RoomsDatabase) keyToSuperStreamVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := r.superStream.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

// Local rooms super stream
//

func (r *RoomsDatabase) rangeForLocalSuperStream(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.localSuperStream, fromVersion, toVersion, roomID.String())
}

func (r *RoomsDatabase) keyForLocalSuperStreamRoom(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := r.localSuperStream.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (r *RoomsDatabase) keyToLocalSuperStreamVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := r.localSuperStream.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

// Super stream tup add/parse
//

func (r *RoomsDatabase) valueToSuperStreamItem(roomID id.RoomID, version tuple.Versionstamp, value []byte) SuperStreamItem {
	tup, _ := tuple.Unpack(value)
	sType := SuperStreamType(tup[0].(string))

	sTup := SuperStreamItem{
		Type:    sType,
		Version: version,
	}

	switch sType {
	case SuperStreamEvent:
		sTup.EventIDTup = &types.EventIDTup{
			RoomID:  roomID,
			EventID: id.EventID(tup[1].(string)),
		}
	case SuperStreamReceipt:
		sTup.Receipt = &types.Receipt{
			RoomID:   roomID,
			UserID:   id.UserID(tup[1].(string)),
			EventID:  id.EventID(tup[2].(string)),
			ThreadID: tup[3].(string),
			Type:     event.ReceiptType(tup[4].(string)),
			Data:     tup[5].([]byte),
		}
	}

	return sTup
}

func (r *RoomsDatabase) txnAddEventToSuperStream(
	txn fdb.Transaction,
	ev *types.Event,
	version tuple.Versionstamp,
) {
	value := tuple.Tuple{string(SuperStreamEvent), ev.ID.String()}.Pack()
	txn.SetVersionstampedKey(r.keyForSuperStreamRoom(ev.RoomID, version), value)

	if ev.Sender.Homeserver() == r.config.ServerName {
		// If the sender is one of our users, also add to the local super stream
		txn.SetVersionstampedKey(r.keyForLocalSuperStreamRoom(ev.RoomID, version), value)
	}
}

func (r *RoomsDatabase) txnAddReceiptToSuperStream(
	txn fdb.Transaction,
	rc *types.Receipt,
	version tuple.Versionstamp,
) {
	value := tuple.Tuple{
		string(SuperStreamReceipt),
		rc.UserID.String(),
		rc.EventID.String(),
		rc.ThreadID,
		string(rc.Type),
		rc.Data,
	}.Pack()

	txn.SetVersionstampedKey(r.keyForSuperStreamRoom(rc.RoomID, version), value)

	if rc.UserID.Homeserver() == r.config.ServerName {
		// If the sender if one of our users, also add to the local super stream
		txn.SetVersionstampedKey(r.keyForLocalSuperStreamRoom(rc.RoomID, version), value)
	}

	if currVersionB := txn.Get(r.keyForSuperStreamReceiptVersion(rc)).MustGet(); currVersionB != nil {
		// Clear any previous receipt in the super stream
		currVersion := types.MustValueToVersionstamp(currVersionB)
		txn.Clear(r.keyForSuperStreamRoom(rc.RoomID, currVersion))
		txn.Clear(r.keyForLocalSuperStreamRoom(rc.RoomID, currVersion))
	}

	// Store reference of roomID/userID/type/threadID -> version
	txn.SetVersionstampedValue(r.keyForSuperStreamReceiptVersion(rc), types.VersionstampToValue(version))
}

// Super stream pagination
//

// Paginate a rooms super stream items for client sync
func (r *RoomsDatabase) txnPaginateRoomSuperStream(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
	limit int,
	eventsProvider *events.TxnEventsProvider,
) ([]SuperStreamItem, error) {
	iter := txn.GetRange(
		r.rangeForSuperStream(roomID, fromVersion, toVersion),
		fdb.RangeOptions{
			Limit: limit,
		},
	).Iterator()

	tups := make([]SuperStreamItem, 0, limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		version := r.keyToSuperStreamVersion(kv.Key)
		tup := r.valueToSuperStreamItem(roomID, version, kv.Value)
		tups = append(tups, tup)
		if eventsProvider != nil && tup.Type == SuperStreamEvent {
			eventsProvider.WillGet(tup.EventIDTup.EventID)
		}
	}

	return tups, nil
}

// Paginate a rooms local super stream - that is the subset of superstream items that originated
// at this homeserver. Used in federation sender workers to "sync" a federated server.
func (r *RoomsDatabase) txnPaginateRoomLocalSuperStream(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
	limit int,
	eventsProvider *events.TxnEventsProvider,
) ([]SuperStreamItem, error) {
	iter := txn.GetRange(
		r.rangeForLocalSuperStream(roomID, fromVersion, toVersion),
		fdb.RangeOptions{
			Limit: limit,
		},
	).Iterator()

	tups := make([]SuperStreamItem, 0, limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		version := r.keyToLocalSuperStreamVersion(kv.Key)
		tup := r.valueToSuperStreamItem(roomID, version, kv.Value)
		tups = append(tups, tup)
		if eventsProvider != nil && tup.Type == SuperStreamEvent {
			eventsProvider.WillGet(tup.EventIDTup.EventID)
		}
	}

	return tups, nil
}
