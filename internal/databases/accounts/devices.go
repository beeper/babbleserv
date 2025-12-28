package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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
