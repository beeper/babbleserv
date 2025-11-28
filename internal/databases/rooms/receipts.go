package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) GetCurrentRoomReceipts(ctx context.Context, roomID id.RoomID) ([]*types.Receipt, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Receipt, error) {
		iter := txn.GetRange(
			r.receipts.RangeForRoomVersion(roomID, types.ZeroVersionstamp, types.ZeroVersionstamp),
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		rcs := make([]*types.Receipt, 0, 5)

		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			rcs = append(rcs, types.MustBytesToReceipt(kv.Value))
		}

		return rcs, nil
	})
}
