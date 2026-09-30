package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) CreateKeyBackupVersion(ctx context.Context, userID id.UserID, version *types.KeyBackupVersion) (string, error) {
	future, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (fdb.FutureKey, error) {
		a.keybackup.TxnStoreVersion(txn, userID, version)
		return txn.GetVersionstamp(), nil
	})
	if err != nil {
		return "", err
	}
	committed, err := future.Get()
	if err != nil {
		return "", err
	}
	return types.MustVersionstampToString(types.DecodeRawVersionstamp(committed)), nil
}

func (a *AccountsDatabase) GetLatestKeyBackupVersion(ctx context.Context, userID id.UserID) (*types.KeyBackupVersionWithMeta, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupVersionWithMeta, error) {
		version, vstamp, err := a.keybackup.TxnGetLatestVersion(txn, userID)
		if err != nil {
			return nil, err
		}
		if version == nil {
			return nil, nil
		}

		count, etag := a.keybackup.TxnGetVersionMeta(txn, userID, vstamp)
		return &types.KeyBackupVersionWithMeta{
			Version:   types.MustVersionstampToString(vstamp),
			Algorithm: version.Algorithm,
			AuthData:  version.AuthData,
			Count:     count,
			ETag:      etag,
		}, nil
	})
}

func (a *AccountsDatabase) GetKeyBackupVersion(ctx context.Context, userID id.UserID, versionString string) (*types.KeyBackupVersionWithMeta, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupVersionWithMeta, error) {
		return a.keybackup.TxnGetVersionWithMeta(txn, userID, vstamp)
	})
}

func (a *AccountsDatabase) UpdateKeyBackupVersion(ctx context.Context, userID id.UserID, versionString string, version *types.KeyBackupVersion) error {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return err
	}

	_, err = util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, a.keybackup.TxnUpdateVersionAuthData(txn, userID, vstamp, version)
	})
	return err
}

func (a *AccountsDatabase) DeleteKeyBackupVersion(ctx context.Context, userID id.UserID, versionString string) error {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return err
	}

	_, err = util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		a.keybackup.TxnDeleteVersion(txn, userID, vstamp)
		return nil, nil
	})
	return err
}

func (a *AccountsDatabase) StoreKeyBackupKeys(ctx context.Context, userID id.UserID, versionString string, rooms map[string]map[string]*types.KeyBackupData) (*types.KeyBackupUpdateResponse, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	// First, do the write transaction
	versionExists, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		// Verify version exists
		version, err := a.keybackup.TxnGetVersion(txn, userID, vstamp)
		if err != nil {
			return false, err
		}
		if version == nil {
			return false, nil // Version not found
		}

		_, current, err := a.keybackup.TxnGetLatestVersion(txn, userID)
		if err != nil {
			return false, err
		}
		if current != vstamp {
			return false, &types.WrongKeyBackupVersionError{CurrentVersion: types.MustVersionstampToString(current)}
		}

		storedCount := 0
		changed := false
		for roomID, sessions := range rooms {
			for sessionID, data := range sessions {
				stored, delta := a.keybackup.TxnStoreKey(txn, userID, vstamp, roomID, sessionID, data)
				changed = changed || stored
				storedCount += delta
			}
		}

		// Update count and etag if any keys were stored
		if changed {
			a.keybackup.TxnBumpVersionMeta(txn, userID, vstamp, storedCount)
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if !versionExists {
		return nil, nil
	}

	// Read the updated count and etag
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupUpdateResponse, error) {
		count, etag := a.keybackup.TxnGetVersionMeta(txn, userID, vstamp)
		return &types.KeyBackupUpdateResponse{
			ETag:  etag,
			Count: count,
		}, nil
	})
}

func (a *AccountsDatabase) GetKeyBackupKey(ctx context.Context, userID id.UserID, versionString, roomID, sessionID string) (*types.KeyBackupData, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupData, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		return a.keybackup.TxnGetKey(txn, userID, vstamp, roomID, sessionID)
	})
}

func (a *AccountsDatabase) GetKeyBackupKeysForRoom(ctx context.Context, userID id.UserID, versionString, roomID string) (map[string]*types.KeyBackupData, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (map[string]*types.KeyBackupData, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		return a.keybackup.TxnGetKeysForRoom(txn, userID, vstamp, roomID)
	})
}

func (a *AccountsDatabase) GetAllKeyBackupKeys(ctx context.Context, userID id.UserID, versionString string) (map[string]map[string]*types.KeyBackupData, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (map[string]map[string]*types.KeyBackupData, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		return a.keybackup.TxnGetAllKeys(txn, userID, vstamp)
	})
}

func (a *AccountsDatabase) DeleteKeyBackupKey(ctx context.Context, userID id.UserID, versionString, roomID, sessionID string) (*types.KeyBackupUpdateResponse, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	_, err = util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		if a.keybackup.TxnDeleteKey(txn, userID, vstamp, roomID, sessionID) {
			a.keybackup.TxnBumpVersionMeta(txn, userID, vstamp, -1)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupUpdateResponse, error) {
		count, etag := a.keybackup.TxnGetVersionMeta(txn, userID, vstamp)
		return &types.KeyBackupUpdateResponse{
			ETag:  etag,
			Count: count,
		}, nil
	})
}

func (a *AccountsDatabase) DeleteKeyBackupKeysForRoom(ctx context.Context, userID id.UserID, versionString, roomID string) (*types.KeyBackupUpdateResponse, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	_, err = util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		deletedCount := a.keybackup.TxnDeleteKeysForRoom(txn, userID, vstamp, roomID)
		if deletedCount > 0 {
			a.keybackup.TxnBumpVersionMeta(txn, userID, vstamp, -deletedCount)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupUpdateResponse, error) {
		count, etag := a.keybackup.TxnGetVersionMeta(txn, userID, vstamp)
		return &types.KeyBackupUpdateResponse{
			ETag:  etag,
			Count: count,
		}, nil
	})
}

func (a *AccountsDatabase) DeleteAllKeyBackupKeys(ctx context.Context, userID id.UserID, versionString string) (*types.KeyBackupUpdateResponse, error) {
	vstamp, err := parseKeyBackupVersion(versionString)
	if err != nil {
		return nil, err
	}

	_, err = util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		if err := a.requireKeyBackupVersion(txn, userID, vstamp); err != nil {
			return nil, err
		}
		deletedCount := a.keybackup.TxnDeleteAllKeys(txn, userID, vstamp)
		if deletedCount > 0 {
			a.keybackup.TxnBumpVersionMeta(txn, userID, vstamp, -deletedCount)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.KeyBackupUpdateResponse, error) {
		count, etag := a.keybackup.TxnGetVersionMeta(txn, userID, vstamp)
		return &types.KeyBackupUpdateResponse{
			ETag:  etag,
			Count: count,
		}, nil
	})
}

// Backup IDs are opaque to clients; malformed or incomplete IDs cannot identify a backup.
func parseKeyBackupVersion(value string) (tuple.Versionstamp, error) {
	version, err := types.StringToVersionstamp(value)
	if err != nil || types.IsIncompleteVersionstamp(version) {
		return types.ZeroVersionstamp, types.ErrKeyBackupNotFound
	}
	return version, nil
}

func (a *AccountsDatabase) requireKeyBackupVersion(txn fdb.ReadTransaction, userID id.UserID, version tuple.Versionstamp) error {
	backup, err := a.keybackup.TxnGetVersion(txn, userID, version)
	if err != nil {
		return err
	}
	if backup == nil {
		return types.ErrKeyBackupNotFound
	}
	return nil
}
