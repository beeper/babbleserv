package accounts

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"golang.org/x/crypto/bcrypt"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var ZeroTime time.Time

func (a *AccountsDatabase) GetLocalUserForUsername(ctx context.Context, username string) (*types.User, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.User, error) {
		return a.users.TxnGetLocalUser(txn, username, a.config.ServerName)
	})
}

func (a *AccountsDatabase) GetUserDeviceForAuthToken(ctx context.Context, token string) (types.UserDevice, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (types.UserDevice, error) {
		authToken, err := a.tokens.TxnGetAuthTokenTup(txn, token)
		var device types.UserDevice
		if err != nil {
			return device, err
		} else if authToken == nil {
			return device, types.ErrUserNotFound
		} else if authToken.Expires.UnixMicro() != 0 && authToken.Expires.Before(time.Now().UTC()) {
			return device, types.ErrTokenExpired
		} else {
			device.UserID = authToken.UserID
			device.DeviceID = authToken.DeviceID
			return device, nil
		}
	})
}

type authResp struct {
	DeviceID     id.DeviceID `json:"device_id"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token,omitempty"`
}

func (a *AccountsDatabase) LoginWithPassword(
	ctx context.Context,
	username, password string,
	withRefreshToken bool,
	deviceID id.DeviceID,
	initialDeviceDisplayName string,
) (authResp, error) {
	if deviceID == "" {
		deviceID = id.DeviceID(util.GenerateRandomStringBase32Hex(8))
	}
	resp := authResp{
		DeviceID: deviceID,
	}

	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (authResp, error) {
		hashedPassword, err := a.users.TxnGetLocalUserPasswordHash(txn, username)
		if err != nil {
			return resp, err
		} else if hashedPassword == nil {
			return resp, types.ErrUserNotFound
		}
		if err = bcrypt.CompareHashAndPassword(hashedPassword, []byte(password)); err != nil {
			return resp, types.ErrInvalidPassword
		}

		userID := id.UserID("@" + username + ":" + a.config.ServerName)
		resp.AccessToken, resp.RefreshToken = a.tokens.TxnCreateNewTokensForUserDevice(
			txn,
			userID,
			deviceID,
			withRefreshToken,
			a.config.Accounts.RefreshAccessTokenExpire,
		)

		if _, err = a.devices.TxnGetOrCreateDevice(txn, userID, deviceID, initialDeviceDisplayName); err != nil {
			return resp, err
		}

		return resp, nil
	})
}

// Registers a user with a given username/password combination, note the username is not checked
// for Matrix localpart validity, caller is responsible.
func (a *AccountsDatabase) RegisterWithPassword(
	ctx context.Context,
	username string,
	password []byte,
	withRefreshToken bool,
	deviceID id.DeviceID,
	initialDeviceDisplayName string,
) (authResp, error) {
	if deviceID == "" {
		deviceID = id.DeviceID(util.GenerateRandomStringBase32Hex(16))
	}
	resp := authResp{
		DeviceID: deviceID,
	}

	hashedPassword, err := bcrypt.GenerateFromPassword(password, 12)
	if err != nil {
		return resp, err
	}

	if _, err = util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (*struct{}, error) {
		user := types.User{
			Username:   username,
			ServerName: a.config.ServerName,
			CreatedAt:  time.Now().UTC(),
		}

		if err := a.users.TxnCreateUser(txn, &user, hashedPassword); err != nil {
			return nil, err
		}

		userID := user.UserID()
		resp.AccessToken, resp.RefreshToken = a.tokens.TxnCreateNewTokensForUserDevice(
			txn,
			userID,
			deviceID,
			withRefreshToken,
			a.config.Accounts.RefreshAccessTokenExpire,
		)

		if _, err = a.devices.TxnGetOrCreateDevice(txn, userID, deviceID, initialDeviceDisplayName); err != nil {
			return nil, err
		}

		return nil, nil
	}); err != nil {
		return resp, err
	}

	return resp, nil
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

	if _, err := util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
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
