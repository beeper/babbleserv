package accounts

import (
	"context"
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) CreateFilter(
	ctx context.Context,
	userID id.UserID,
	filter mautrix.Filter,
) ([]byte, error) {
	versionFut, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (fdb.FutureKey, error) {
		v := tuple.IncompleteVersionstamp(0)
		key := a.users.KeyForNewUserFilter(userID.Localpart(), v)
		b, err := json.Marshal(filter)
		if err != nil {
			return nil, err
		}
		txn.SetVersionstampedKey(key, b)
		return txn.GetVersionstamp(), nil
	})
	if err != nil {
		return nil, err
	}

	// Get the saved version, re-encode it as a tuple w/VersionstampToBytes
	b := versionFut.MustGet()
	v := types.DecodeRawVersionstamp(b)
	return types.MustVersionstampToBytes(v), nil
}

func (a *AccountsDatabase) GetFilter(
	ctx context.Context,
	userID id.UserID,
	filterID []byte,
) (*mautrix.Filter, error) {
	version := types.MustBytesToVersionstamp(filterID)

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*mautrix.Filter, error) {
		key := a.users.KeyForUserFilter(userID.Localpart(), version)
		b := txn.Get(key).MustGet()

		var filter mautrix.Filter
		if err := json.Unmarshal(b, &filter); err != nil {
			return nil, err
		}

		return &filter, nil
	})
}
