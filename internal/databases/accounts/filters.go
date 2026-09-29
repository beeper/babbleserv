package accounts

import (
	"context"
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) CreateFilter(
	ctx context.Context,
	userID id.UserID,
	filter json.RawMessage,
) ([]byte, error) {
	versionFut, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (fdb.FutureKey, error) {
		v := tuple.IncompleteVersionstamp(0)
		if err := a.users.TxnStoreUserFilter(txn, userID, filter, v); err != nil {
			return nil, err
		}
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
) (json.RawMessage, error) {
	// Only decode one complete versionstamp; malformed tuple input can panic.
	if len(filterID) != 13 || filterID[0] != 0x33 {
		return nil, nil
	}
	version, err := types.BytesToVersionstamp(filterID)
	if err != nil || types.IsIncompleteVersionstamp(version) {
		return nil, nil
	}
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (json.RawMessage, error) {
		return a.users.TxnGetUserFilter(txn, userID, version)
	})
}
