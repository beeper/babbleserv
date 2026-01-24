package accounts

import (
	"context"
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) SyncAccountsForuser(
	ctx context.Context,
	userID id.UserID,
	fromVersion tuple.Versionstamp,
	options types.SyncOptions,
) (tuple.Versionstamp, map[types.AccountDataTup]map[string]any, *pushrules.PushRuleset, error) {
	var ads map[types.AccountDataTup]map[string]any
	var latestVersion tuple.Versionstamp
	var ruleset *pushrules.PushRuleset

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
			kv := iter.MustGet()
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

		var fetchRules bool
		if fromVersion == types.ZeroVersionstamp {
			fetchRules = true
		} else {
			pushVersion := a.pushrules.TxnGetUserPushVersion(txn, userID)
			if pushVersion == types.ZeroVersionstamp || (types.VersionIsAfter(pushVersion, fromVersion) && types.VersionIsAtOrBefore(pushVersion, latestVersion)) {
				fetchRules = true
			}
		}
		if fetchRules {
			rules, err := a.pushrules.TxnGetRulesForUser(txn, userID)
			if err != nil {
				return nil, err
			}
			ruleset = rules
		}

		return nil, nil
	})

	return latestVersion, ads, ruleset, err
}
