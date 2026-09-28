package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) GetEventTransactionID(
	ctx context.Context,
	eventID id.EventID,
	device types.UserDevice,
) (string, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (string, error) {
		return r.txnGetEventTransactionID(txn, eventID, device), nil
	})
}

func (r *RoomsDatabase) keyForEventTransaction(device types.UserDevice, roomID id.RoomID, endpoint, txnID string) fdb.Key {
	return r.eventTransactions.Pack(tuple.Tuple{
		device.UserID.String(), device.DeviceID.String(), roomID.String(), endpoint, txnID,
	})
}

func (r *RoomsDatabase) keyForEventTransactionID(eventID id.EventID, device types.UserDevice) fdb.Key {
	return r.eventTransactionIDs.Pack(tuple.Tuple{
		eventID.String(), device.UserID.String(), device.DeviceID.String(),
	})
}

func (r *RoomsDatabase) txnGetEventForTransaction(
	txn fdb.ReadTransaction,
	device types.UserDevice,
	roomID id.RoomID,
	endpoint string,
	txnID string,
) *types.Event {
	eventID := txn.Get(r.keyForEventTransaction(device, roomID, endpoint, txnID)).MustGet()
	if eventID == nil {
		return nil
	}
	return r.events.TxnGetEvent(txn, id.EventID(eventID))
}

func (r *RoomsDatabase) txnStoreEventTransaction(
	txn fdb.Transaction,
	device types.UserDevice,
	roomID id.RoomID,
	endpoint string,
	txnID string,
	eventID id.EventID,
) {
	txn.Set(r.keyForEventTransaction(device, roomID, endpoint, txnID), []byte(eventID))
	txn.Set(r.keyForEventTransactionID(eventID, device), []byte(txnID))
}

func (r *RoomsDatabase) txnGetEventTransactionID(
	txn fdb.ReadTransaction,
	eventID id.EventID,
	device types.UserDevice,
) string {
	return string(txn.Get(r.keyForEventTransactionID(eventID, device)).MustGet())
}
