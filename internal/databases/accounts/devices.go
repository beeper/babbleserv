package accounts

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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
