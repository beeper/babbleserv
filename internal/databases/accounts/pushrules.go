package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) GetPushRulesForUser(ctx context.Context, userID id.UserID) (*pushrules.PushRuleset, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*pushrules.PushRuleset, error) {
		return a.pushrules.TxnGetRulesForUser(txn, userID)
	})
}

func (a *AccountsDatabase) GetPushRulesForUserByKind(ctx context.Context, userID id.UserID, kind pushrules.PushRuleType) ([]*pushrules.PushRule, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]*pushrules.PushRule, error) {
		return a.pushrules.TxnGetRulesForUserByKind(txn, userID, kind)
	})
}

func (a *AccountsDatabase) GetPushRuleForUser(ctx context.Context, userID id.UserID, kind pushrules.PushRuleType, ruleID string) (*pushrules.PushRule, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*pushrules.PushRule, error) {
		return a.pushrules.TxnGetRuleForUser(txn, userID, kind, ruleID)
	})
}

func (a *AccountsDatabase) PutPushRuleForUser(ctx context.Context, userID id.UserID, kind pushrules.PushRuleType, ruleID string, rule *types.StoredPushRule) error {
	_, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		a.pushrules.TxnPutRuleForUser(txn, userID, kind, ruleID, rule)
		return nil, nil
	})
	if err == nil {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
	}
	return err
}

func (a *AccountsDatabase) DeletePushRuleForUser(ctx context.Context, userID id.UserID, kind pushrules.PushRuleType, ruleID string) error {
	_, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		a.pushrules.TxnDeleteRuleForUser(txn, userID, kind, ruleID)
		return nil, nil
	})
	if err == nil {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
	}
	return err
}

func (a *AccountsDatabase) GetUserPushRulesVersion(ctx context.Context, userID id.UserID) (tuple.Versionstamp, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (tuple.Versionstamp, error) {
		return a.pushrules.TxnGetUserPushVersion(txn, userID), nil
	})
}
