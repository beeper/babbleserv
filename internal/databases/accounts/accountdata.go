package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) SetAccountData(ctx context.Context, ads []*types.AccountData) error {
	_, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		for i, ad := range ads {
			versionKey := a.accountdata.KeyForAccountDataVersion(ad.AccountDataTup)
			prevVersion := types.ZeroVersionstamp
			if b := txn.Get(versionKey).MustGet(); b != nil {
				prevVersion = types.MustBytesToVersionstamp(b)
			}

			version := tuple.IncompleteVersionstamp(uint16(i))

			// Add to user version, remove any old version
			txn.SetVersionstampedKey(a.accountdata.KeyForUserVersion(ad.UserID, version), ad.ToBytes())
			if prevVersion != types.ZeroVersionstamp {
				txn.Clear(a.accountdata.KeyForUserVersion(ad.UserID, prevVersion))
			}

			// Update version key
			txn.SetVersionstampedValue(versionKey, types.MustVersionstampToBytes(version))
		}
		return nil, nil
	})
	return err
}

func (a *AccountsDatabase) GetAccountData(
	ctx context.Context,
	userID id.UserID,
	roomID id.RoomID,
	adType event.Type,
) (*types.AccountData, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.AccountData, error) {
		versionBytes := txn.Get(a.accountdata.KeyForAccountDataVersion(types.AccountDataTup{
			UserID: userID,
			RoomID: roomID,
			Type:   adType,
		})).MustGet()
		if versionBytes == nil {
			return nil, nil
		}
		version := types.MustBytesToVersionstamp(versionBytes)

		adBytes := txn.Get(a.accountdata.KeyForUserVersion(userID, version)).MustGet()
		if adBytes == nil {
			// This should never happen!
			panic("got nil account data but we have a version!")
		}

		return types.BytesToAccountData(adBytes)
	})
}
