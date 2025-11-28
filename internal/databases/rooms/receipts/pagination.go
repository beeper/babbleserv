package receipts

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (r *ReceiptsDirectory) TxnPaginateRoomReceipts(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
) ([]*types.ReceiptWithVersion, error) {
	iter := txn.GetRange(
		r.RangeForRoomVersion(roomID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	rcs := make([]*types.ReceiptWithVersion, 0, 5)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		rcs = append(rcs, &types.ReceiptWithVersion{
			Version: r.KeyToRoomVersion(kv.Key),
			Receipt: *types.MustBytesToReceipt(kv.Value),
		})
	}

	return rcs, nil
}

func (r *ReceiptsDirectory) TxnPaginateLocalRoomReceipts(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
) ([]*types.ReceiptWithVersion, error) {
	iter := txn.GetRange(
		r.RangeForLocalRoomVersion(roomID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	rcs := make([]*types.ReceiptWithVersion, 0, 5)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		rcs = append(rcs, &types.ReceiptWithVersion{
			Version: r.KeyToLocalRoomVersion(kv.Key),
			Receipt: *types.MustBytesToReceipt(kv.Value),
		})
	}

	return rcs, nil
}

func (r *ReceiptsDirectory) TxnPaginateUserPrivateReceipts(
	txn fdb.ReadTransaction,
	userID id.UserID,
	options types.PaginationOptions,
) ([]*types.ReceiptWithVersion, error) {
	iter := txn.GetRange(
		r.RangeForUserVersion(userID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	rcs := make([]*types.ReceiptWithVersion, 0, 5)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		rcs = append(rcs, &types.ReceiptWithVersion{
			Version: r.KeyToUserVersion(kv.Key),
			Receipt: *types.MustBytesToReceipt(kv.Value),
		})
	}

	return rcs, nil
}
