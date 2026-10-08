package accounts

import (
	"context"
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) PaginateDeviceChanges(
	ctx context.Context,
	options types.PaginationOptions,
) ([]types.UserDeviceChange, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]types.UserDeviceChange, error) {
		return a.devices.TxnPaginateDeviceChanges(txn, options)
	})
}

func (a *AccountsDatabase) txnStoreDeviceListChange(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, index uint16) error {
	stream, err := a.users.TxnAllocateDeviceListVersion(txn, userID)
	if err != nil {
		return err
	}
	a.devices.TxnStoreDeviceListChange(txn, userID, deviceID, tuple.IncompleteVersionstamp(index), stream)
	return nil
}

func (a *AccountsDatabase) txnGetOrCreateDevice(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	initialDisplayName string,
) error {
	_, created, err := a.devices.TxnGetOrCreateDevice(txn, userID, deviceID, initialDisplayName)
	if err != nil || !created {
		return err
	}
	return a.txnStoreDeviceListChange(txn, userID, deviceID, 0)
}

func (a *AccountsDatabase) GetDeviceListUpdate(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
) (types.LocalDeviceListUpdate, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (types.LocalDeviceListUpdate, error) {
		var update types.LocalDeviceListUpdate
		getUser, err := a.users.TxnGetLocalUserFuture(txn, userID)
		if err != nil {
			return update, err
		}
		device, keys, err := a.devices.TxnGetDeviceWithKeys(txn, userID, deviceID)
		if err != nil {
			return update, err
		}
		user, err := getUser()
		if err != nil {
			return update, err
		} else if user == nil {
			return update, fmt.Errorf("%w: %s", types.ErrUserNotFound, userID)
		}

		update.Version = user.DeviceListVersion
		update.Device = device
		update.Keys = keys
		if keys == nil {
			return update, nil
		}

		deviceKeyID := id.KeyID(deviceID)
		ownerSignatures := a.txnGetOwnerSignatures(txn, userID, []id.KeyID{deviceKeyID})
		update.Keys.Signatures = withOwnerSignatures(update.Keys.Signatures, userID, ownerSignatures[deviceKeyID])
		return update, nil
	})
}

// Nil when the local user does not exist
func (a *AccountsDatabase) GetLocalUserDevicesSnapshot(ctx context.Context, userID id.UserID) (*types.LocalDeviceSnapshot, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.LocalDeviceSnapshot, error) {
		user, err := a.users.TxnGetLocalUser(txn, userID)
		if err != nil || user == nil {
			return nil, err
		}
		devices, err := a.devices.TxnGetUserDevicesWithKeys(txn, userID)
		if err != nil {
			return nil, err
		}
		crossSigningKeys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
		if err != nil {
			return nil, err
		}

		keyIDs := make([]id.KeyID, 0, len(devices)+2)
		for _, device := range devices {
			if device.Keys != nil {
				keyIDs = append(keyIDs, id.KeyID(device.Device.ID))
			}
		}
		if crossSigningKeys != nil {
			keyIDs = append(keyIDs, crossSigningKeys.Master.KeyID(), crossSigningKeys.SelfSigning.KeyID())
		}
		ownerSignatures := a.txnGetOwnerSignatures(txn, userID, keyIDs)

		for _, device := range devices {
			if device.Keys != nil {
				device.Keys.Signatures = withOwnerSignatures(device.Keys.Signatures, userID, ownerSignatures[id.KeyID(device.Device.ID)])
			}
		}

		snapshot := &types.LocalDeviceSnapshot{
			Version: user.DeviceListVersion,
			Devices: devices,
		}
		if crossSigningKeys != nil {
			snapshot.MasterKey = ownerSignedCrossSigningKey(crossSigningKeys.Master, userID, ownerSignatures)
			snapshot.SelfSigningKey = ownerSignedCrossSigningKey(crossSigningKeys.SelfSigning, userID, ownerSignatures)
		}
		return snapshot, nil
	})
}

func (a *AccountsDatabase) GetUserDevice(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
) (*types.Device, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.Device, error) {
		return a.devices.TxnGetDevice(txn, userID, deviceID)
	})
}

func (a *AccountsDatabase) GetUserDevices(
	ctx context.Context,
	userID id.UserID,
) ([]*types.Device, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]*types.Device, error) {
		iter := txn.GetRange(a.devices.RangeForUserDevices(userID), fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		}).Iterator()

		devices := make([]*types.Device, 0, 5)
		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			devices = append(devices, types.MustNewDeviceFromBytes(kv.Value))
		}
		return devices, nil
	})
}

func (a *AccountsDatabase) UpdateUserDevice(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	displayName string,
) error {
	changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		device, err := a.devices.TxnGetDevice(txn, userID, deviceID)
		if err != nil {
			return false, err
		} else if device == nil {
			return false, types.ErrUserDeviceNotFound
		}

		if device.DisplayName == displayName {
			return false, nil
		}

		// Store the change
		device.DisplayName = displayName
		a.devices.TxnStoreDevice(txn, userID, device)

		if err := a.txnStoreDeviceListChange(txn, userID, deviceID, 0); err != nil {
			return false, err
		}
		return true, nil
	})

	if err != nil {
		return err
	} else if changed {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
		log.Info().Msg("Updated device")
	} else {
		log.Warn().Msg("Ignored no change device update")
	}
	return err
}

type deviceTokens struct {
	AuthTokens    map[id.DeviceID][]string
	RefreshTokens map[id.DeviceID][]string
}

func (a *AccountsDatabase) GetUserDeviceTokenPrefixes(
	ctx context.Context,
	userID id.UserID,
) (deviceTokens, error) {
	var tokens deviceTokens
	var err error

	if _, err := util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		tokens.AuthTokens, err = a.tokens.TxnListUserDeviceAuthTokenPrefixes(txn, userID)
		if err != nil {
			return nil, err
		}
		tokens.RefreshTokens, err = a.tokens.TxnListUserDeviceRefreshTokenPrefixes(txn, userID)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}); err != nil {
		return tokens, err
	} else {
		return tokens, nil
	}
}

func (a *AccountsDatabase) txnDeleteUserDevices(txn fdb.Transaction, userID id.UserID, deviceIDs []id.DeviceID) (bool, error) {
	if len(deviceIDs) > types.MaxVersionstampUserVersion {
		return false, fmt.Errorf("too many devices in one deletion: %d", len(deviceIDs))
	}
	seen := make(map[id.DeviceID]struct{}, len(deviceIDs))
	changed := false
	for index, deviceID := range deviceIDs {
		if _, ok := seen[deviceID]; ok {
			continue
		}
		seen[deviceID] = struct{}{}
		device, err := a.devices.TxnGetDevice(txn, userID, deviceID)
		if err != nil {
			return false, err
		}
		if device == nil {
			continue
		}
		if err := a.tokens.TxnClearUserDeviceTokens(txn, userID, deviceID); err != nil {
			return false, err
		}
		if err := a.tokens.TxnClearDeviceUIASessions(txn, userID, deviceID); err != nil {
			return false, err
		}
		a.devices.TxnDeleteDevice(txn, userID, deviceID)
		if err := a.txnStoreDeviceListChange(txn, userID, deviceID, uint16(index)); err != nil {
			return false, err
		}
		changed = true
	}
	if err := a.users.TxnDeletePushersForDevices(txn, userID, seen); err != nil {
		return false, err
	}
	return changed, nil
}

func (a *AccountsDatabase) DeleteUserDevicesWithPassword(
	ctx context.Context,
	userDevice types.UserDevice,
	password string,
	deviceIDs []id.DeviceID,
	uiaSession, method, path string,
) error {
	err := a.runAccountUpdate(ctx, userDevice.UserID, &password, func(txn fdb.Transaction) (bool, error) {
		if err := a.txnConsumeUIASession(txn, uiaSession, userDevice, method, path); err != nil {
			return false, err
		}
		return a.txnDeleteUserDevices(txn, userDevice.UserID, deviceIDs)
	})
	if errors.Is(err, types.ErrUIASessionExpired) {
		a.cleanupExpiredUIASession(ctx, uiaSession)
	}
	return err
}

func (a *AccountsDatabase) Logout(ctx context.Context, userDevice types.UserDevice, all bool) error {
	return a.runAccountUpdate(ctx, userDevice.UserID, nil, func(txn fdb.Transaction) (bool, error) {
		deviceIDs := []id.DeviceID{userDevice.DeviceID}
		if all {
			devices, err := txn.GetRange(a.devices.RangeForUserDevices(userDevice.UserID), fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll, Limit: types.MaxVersionstampUserVersion + 1,
			}).GetSliceWithError()
			if err != nil {
				return false, err
			}
			deviceIDs = make([]id.DeviceID, len(devices))
			for i, kv := range devices {
				deviceIDs[i] = types.MustNewDeviceFromBytes(kv.Value).ID
			}
		}
		return a.txnDeleteUserDevices(txn, userDevice.UserID, deviceIDs)
	})
}
