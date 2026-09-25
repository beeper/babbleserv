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

func (a *AccountsDatabase) StoreDeviceChange(ctx context.Context, userID id.UserID, deviceID id.DeviceID) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		version := tuple.IncompleteVersionstamp(0)
		a.devices.TxnStoreDeviceChange(txn, userID, deviceID, version)
		return nil, nil
	})
	return err
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

		// And send a device change so this gets sent out to relevant users/servers
		version := tuple.IncompleteVersionstamp(0)
		a.devices.TxnStoreDeviceChange(txn, userID, deviceID, version)

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
		a.devices.TxnStoreDeviceChange(txn, userID, deviceID, tuple.IncompleteVersionstamp(uint16(index)))
		if err := a.users.TxnIncrementUserDeviceListVersion(txn, userID); err != nil {
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
