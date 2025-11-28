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

func (a *AccountsDatabase) SyncAccountsForuser(
	ctx context.Context,
	userID id.UserID,
	fromVersion tuple.Versionstamp,
	options types.SyncOptions,
) (tuple.Versionstamp, map[types.AccountDataTup]map[string]any, error) {
	var ads map[types.AccountDataTup]map[string]any
	var latestVersion tuple.Versionstamp

	_, err := util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		latestVersion = util.TxnGetLatestWriteVersion(txn)
		iter := txn.GetRange(
			a.accountdata.RangeForUserVersion(userID, fromVersion, latestVersion),
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		ads = make(map[types.AccountDataTup]map[string]any, 10)

		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			ad, err := types.BytesToAccountData(kv.Value)
			if err != nil {
				return nil, err
			}
			var data map[string]any
			if err := json.Unmarshal(ad.Content, &data); err != nil {
				return nil, err
			}
			ads[ad.AccountDataTup] = data
		}

		return nil, nil
	})

	return latestVersion, ads, err
}
