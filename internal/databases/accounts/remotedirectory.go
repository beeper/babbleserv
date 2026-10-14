package accounts

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) EnsureRemoteDirectoryUsers(
	ctx context.Context,
	sources []types.RemoteUserDirectorySource,
	now, lookupAt time.Time,
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, a.users.TxnEnsureRemoteDirectoryUsers(txn, sources, now, lookupAt)
	})
	return err
}

// EnsureUnindexedRemoteDirectoryUsers is EnsureRemoteDirectoryUsers for those of the users with
// neither a profile fetch queued nor a profile fetched, as when indexing every member of a room
func (a *AccountsDatabase) EnsureUnindexedRemoteDirectoryUsers(
	ctx context.Context,
	sources []types.RemoteUserDirectorySource,
	now, lookupAt time.Time,
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, a.users.TxnEnsureUnindexedRemoteDirectoryUsers(txn, sources, now, lookupAt)
	})
	return err
}

func (a *AccountsDatabase) NextRemoteDirectoryProfileJob(
	ctx context.Context,
	now time.Time,
) (*types.RemoteUserDirectoryProfileJob, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.RemoteUserDirectoryProfileJob, error) {
		return a.users.TxnNextRemoteDirectoryProfileJob(txn, now)
	})
}

func (a *AccountsDatabase) FinishRemoteDirectoryProfileJob(
	ctx context.Context,
	job types.RemoteUserDirectoryProfileJob,
	profile *types.UserProfile,
	retryAt *time.Time,
) (bool, error) {
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		return a.users.TxnFinishRemoteDirectoryProfileJob(txn, job, profile, retryAt)
	})
}
