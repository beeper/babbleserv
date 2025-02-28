package receipts

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type ReceiptsDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	byRoomTypeThread subspace.Subspace
}

func NewReceiptsDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ReceiptsDirectory {
	receiptsDir, err := parentDir.CreateOrOpen(db, []string{"receipts"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "receipts").Logger()
	log.Debug().
		Bytes("prefix", receiptsDir.Bytes()).
		Msg("Init rooms/receipts directory")

	return &ReceiptsDirectory{
		log: log,
		db:  db,

		byRoomTypeThread: receiptsDir.Sub("rt"),
	}
}

func (r *ReceiptsDirectory) KeyValueForReceipt(rc *types.Receipt) fdb.KeyValue {
	return fdb.KeyValue{
		Key:   r.byRoomTypeThread.Pack(tuple.Tuple{rc.RoomID.String(), string(rc.Type), rc.ThreadID, rc.UserID.String()}),
		Value: tuple.Tuple{rc.EventID.String(), rc.Data}.Pack(),
	}
}

func (r *ReceiptsDirectory) KeyValueToReceipt(kv fdb.KeyValue) *types.Receipt {
	keyTup, _ := r.byRoomTypeThread.Unpack(kv.Key)
	valTup, _ := tuple.Unpack(kv.Value)

	return &types.Receipt{
		RoomID:   id.RoomID(keyTup[0].(string)),
		Type:     event.ReceiptType(keyTup[1].(string)),
		ThreadID: keyTup[2].(string),
		UserID:   id.UserID(keyTup[3].(string)),

		EventID: id.EventID(valTup[0].(string)),
		Data:    valTup[1].([]byte),
	}
}

func (r *ReceiptsDirectory) rangeForRoomReceipts(roomID id.RoomID, rType event.ReceiptType) fdb.ExactRange {
	return r.byRoomTypeThread.Sub(roomID.String(), string(rType))
}

func (r *ReceiptsDirectory) TxnGetCurrentReceiptsForRoom(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	rType event.ReceiptType,
) ([]*types.Receipt, error) {
	iter := txn.GetRange(
		r.rangeForRoomReceipts(roomID, rType),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	receipts := make([]*types.Receipt, 0)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, r.KeyValueToReceipt(kv))
	}

	return receipts, nil
}
