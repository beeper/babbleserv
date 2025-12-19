package accounts

import (
	"context"
	"reflect"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) GetUserProfile(ctx context.Context, userID id.UserID) (*types.UserProfile, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.UserProfile, error) {
		return a.users.TxnGetUserProfile(txn, userID)
	})
}

func (a *AccountsDatabase) UpdateUserProfile(ctx context.Context, userID id.UserID, key string, value any) error {
	if _, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		profile, err := a.users.TxnGetUserProfile(txn, userID)
		if err != nil {
			return nil, err
		} else if profile == nil {
			profile = &types.UserProfile{}
		}

		switch key {
		case "displayname":
			if profile.DisplayName == value {
				return nil, nil
			}
			profile.DisplayName = value.(string)
		case "avatar_url":
			if profile.AvatarURL == value {
				return nil, nil
			}
			profile.AvatarURL = value.(string)
		default:
			if reflect.DeepEqual(profile.Custom[key], value) {
				return nil, nil
			}
			profile.Custom[key] = value
		}

		a.users.TxnStoreUserProfile(txn, userID, profile)

		version := tuple.IncompleteVersionstamp(0)
		a.users.TxnStoreProfileChange(txn, userID, profile, version)
		return nil, nil
	}); err != nil {
		return err
	} else {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
		return nil
	}
}

func (a *AccountsDatabase) PaginateProfileChanges(
	ctx context.Context,
	options types.PaginationOptions,
) ([]types.UserProfileChange, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]types.UserProfileChange, error) {
		return a.users.TxnPaginateProfileChanges(txn, options)
	})
}

func (a *AccountsDatabase) ClearProfileChanges(
	ctx context.Context,
	toVersion tuple.Versionstamp,
	checkUpdateLock func(fdb.Transaction),
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		checkUpdateLock(txn)
		a.users.TxnClearProfileChanges(txn, toVersion)
		return nil, nil
	})
	return err
}
