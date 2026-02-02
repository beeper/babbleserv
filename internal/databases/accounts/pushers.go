package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules/pushgateway"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) GetPushersForUser(
	ctx context.Context,
	userID id.UserID,
) ([]pushgateway.Pusher, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]pushgateway.Pusher, error) {
		return a.users.TxnGetPushersForUser(txn, userID)
	})
}

func (a *AccountsDatabase) SetPusherForUser(
	ctx context.Context,
	userID id.UserID,
	pusher *pushgateway.Pusher,
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, a.users.TxnSetPusherForUser(txn, userID, pusher)
	})
	return err
}

func (a *AccountsDatabase) DeletePusherForUser(
	ctx context.Context,
	userID id.UserID,
	pushKey string,
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		a.users.TxnDeletePusherForUser(txn, userID, pushKey)
		return nil, nil
	})
	return err
}
