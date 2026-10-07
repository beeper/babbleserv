package accounts

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/accounts/tokens"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var ZeroTime time.Time

const deviceIDBytes = 8

func generateDeviceID() id.DeviceID {
	return id.DeviceID(util.GenerateRandomStringBase32Hex(deviceIDBytes))
}

func (a *AccountsDatabase) GetLocalUser(ctx context.Context, userID id.UserID) (*types.User, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.User, error) {
		return a.users.TxnGetLocalUser(txn, userID)
	})
}

func (a *AccountsDatabase) IsLocalUsernameAvailable(ctx context.Context, username string) (bool, error) {
	userID := id.NewUserID(username, a.config.ServerName)
	user, err := a.GetLocalUser(ctx, userID)
	return user == nil, err
}

func (a *AccountsDatabase) GetUserDeviceForAuthToken(ctx context.Context, token string) (types.UserDevice, error) {
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.UserDevice, error) {
		authToken, err := a.tokens.TxnGetAuthTokenTup(txn, token)
		var device types.UserDevice
		if err != nil {
			return device, err
		} else if authToken == nil {
			return device, types.ErrUserNotFound
		} else if authToken.Expires.UnixMicro() != 0 && authToken.Expires.Before(time.Now().UTC()) {
			return device, types.ErrTokenExpired
		} else {
			if err := a.tokens.TxnMarkAuthTokenUsed(txn, token, authToken); err != nil {
				return device, err
			}
			device.UserID = authToken.UserID
			device.DeviceID = authToken.DeviceID
			return device, nil
		}
	})
}

const uiaSessionLifetime = 5 * time.Minute

func (a *AccountsDatabase) CreateUIASession(
	ctx context.Context,
	userDevice types.UserDevice,
	method, path string,
	request []byte,
) (string, error) {
	session := util.GenerateRandomString(32)
	expiresAt := time.Now().UTC().Add(uiaSessionLifetime)
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		a.tokens.TxnCreateUIASession(
			txn, session, userDevice.UserID, userDevice.DeviceID, method, path, request, expiresAt,
		)
		return nil, nil
	})
	return session, err
}

func (a *AccountsDatabase) GetUIASessionRequest(
	ctx context.Context,
	session string,
	userDevice types.UserDevice,
	method, path string,
) ([]byte, error) {
	type result struct {
		request []byte
		expired bool
	}
	res, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (result, error) {
		uia, err := a.tokens.TxnGetUIASession(txn, session)
		if err != nil {
			return result{}, err
		} else if uia == nil {
			return result{}, types.ErrUIASessionNotFound
		} else if uia.ExpiresAt.Before(time.Now().UTC()) {
			if err := a.tokens.TxnDeleteUIASession(txn, session); err != nil {
				return result{}, err
			}
			return result{expired: true}, nil
		} else if uia.UserID != userDevice.UserID || uia.DeviceID != userDevice.DeviceID ||
			uia.Method != method || uia.Path != path {
			return result{}, types.ErrUIASessionMismatch
		}
		return result{request: uia.Request}, nil
	})
	if err != nil {
		return nil, err
	} else if res.expired {
		return nil, types.ErrUIASessionExpired
	}
	return res.request, nil
}

func (a *AccountsDatabase) txnConsumeUIASession(
	txn fdb.Transaction,
	session string,
	userDevice types.UserDevice,
	method, path string,
) error {
	if session == "" {
		return nil
	}
	uia, err := a.tokens.TxnGetUIASession(txn, session)
	if err != nil {
		return err
	} else if uia == nil {
		return types.ErrUIASessionNotFound
	} else if uia.ExpiresAt.Before(time.Now().UTC()) {
		return types.ErrUIASessionExpired
	} else if uia.UserID != userDevice.UserID || uia.DeviceID != userDevice.DeviceID ||
		uia.Method != method || uia.Path != path {
		return types.ErrUIASessionMismatch
	}
	return a.tokens.TxnDeleteUIASession(txn, session)
}

func (a *AccountsDatabase) cleanupExpiredUIASession(ctx context.Context, session string) {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (types.Nil, error) {
		uia, err := a.tokens.TxnGetUIASession(txn, session)
		if err != nil || uia == nil || !uia.ExpiresAt.Before(time.Now().UTC()) {
			return nil, err
		}
		return nil, a.tokens.TxnDeleteUIASession(txn, session)
	})
	if err != nil {
		a.log.Warn().Err(err).Str("session", session).Msg("Failed to delete expired UIA session")
	}
}

func (a *AccountsDatabase) CleanupExpiredUIASessions(ctx context.Context, limit int) (int, error) {
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (int, error) {
		return a.tokens.TxnCleanupExpiredUIASessions(txn, time.Now().UTC(), limit)
	})
}

type authResp struct {
	UserID       id.UserID   `json:"user_id"`
	DeviceID     id.DeviceID `json:"device_id,omitempty"`
	AccessToken  string      `json:"access_token,omitempty"`
	RefreshToken string      `json:"refresh_token,omitempty"`
	ExpiresInMS  int64       `json:"expires_in_ms,omitempty"`
}

func (a *AccountsDatabase) LoginWithPassword(
	ctx context.Context,
	username, password string,
	withRefreshToken bool,
	deviceID id.DeviceID,
	initialDeviceDisplayName string,
) (authResp, error) {
	// TODO: check if deviceID is base64 -> correct bytes for ed25519 -> reject, just don't allow
	// ed25519 keys base64'd as deviceIDs

	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	resp := authResp{
		DeviceID: deviceID,
	}

	resp, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (authResp, error) {
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
		resp.UserID = userID

		resp.AccessToken, resp.RefreshToken = a.tokens.TxnCreateNewTokensForUserDevice(
			txn,
			userID,
			deviceID,
			withRefreshToken,
			a.config.Accounts.RefreshAccessTokenExpire,
		)

		if expiry := a.config.Accounts.RefreshAccessTokenExpire; withRefreshToken && expiry > 0 {
			resp.ExpiresInMS = max(expiry.Milliseconds(), 1)
		}

		if err := a.txnGetOrCreateDevice(txn, userID, deviceID, initialDeviceDisplayName); err != nil {
			return resp, err
		}

		return resp, nil
	})

	if err == nil {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{resp.UserID},
		})
	}
	return resp, err
}

// Registers a user with a given username/password combination, note the username is not checked
// for Matrix localpart validity, caller is responsible.
func (a *AccountsDatabase) RegisterWithPasswordHash(
	ctx context.Context,
	username string,
	hashedPassword []byte,
	withRefreshToken bool,
	deviceID id.DeviceID,
	initialDeviceDisplayName string,
	inhibitLogin bool,
	uiaSession, method, path string,
) (authResp, error) {
	if !inhibitLogin && deviceID == "" {
		deviceID = generateDeviceID()
	}
	user := types.User{
		Username:   username,
		ServerName: a.config.ServerName,
		CreatedAt:  time.Now().UTC(),
	}
	resp, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (authResp, error) {
		resp := authResp{UserID: user.UserID()}
		if err := a.txnConsumeUIASession(txn, uiaSession, types.UserDevice{}, method, path); err != nil {
			return resp, err
		}
		if err := a.users.TxnCreateLocalUser(txn, &user, hashedPassword); err != nil {
			return resp, err
		}
		if !inhibitLogin {
			resp.DeviceID = deviceID
			resp.AccessToken, resp.RefreshToken = a.tokens.TxnCreateNewTokensForUserDevice(
				txn, resp.UserID, deviceID, withRefreshToken, a.config.Accounts.RefreshAccessTokenExpire,
			)
			if expiry := a.config.Accounts.RefreshAccessTokenExpire; withRefreshToken && expiry > 0 {
				resp.ExpiresInMS = max(expiry.Milliseconds(), 1)
			}
			if err := a.txnGetOrCreateDevice(txn, resp.UserID, deviceID, initialDeviceDisplayName); err != nil {
				return resp, err
			}
		}
		return resp, nil
	})
	if err != nil {
		return resp, err
	}

	a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{resp.UserID}})
	zerolog.Ctx(ctx).
		Info().
		Str("username", username).
		Str("device_id", resp.DeviceID.String()).
		Msg("Registered new user")
	return resp, nil
}

func (a *AccountsDatabase) runAccountUpdate(
	ctx context.Context,
	userID id.UserID,
	password *string,
	callback func(fdb.Transaction) (bool, error),
) error {
	var expectedHash []byte
	if password != nil {
		var err error
		expectedHash, err = util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]byte, error) {
			return a.users.TxnGetLocalUserPasswordHash(txn, userID.Localpart())
		})
		if err != nil {
			return err
		}
		if expectedHash == nil || bcrypt.CompareHashAndPassword(expectedHash, []byte(*password)) != nil {
			return types.ErrInvalidPassword
		}
	}
	changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		if password != nil {
			currentHash, err := a.users.TxnGetLocalUserPasswordHash(txn, userID.Localpart())
			if err != nil {
				return false, err
			}
			if !bytes.Equal(currentHash, expectedHash) {
				return false, types.ErrInvalidPassword
			}
		}
		return callback(txn)
	})
	if err == nil && changed {
		a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{userID}})
	}
	return err
}

type refreshResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresInMS  *int64 `json:"expires_in_ms,omitempty"`
}

func (a *AccountsDatabase) RefreshAccessToken(ctx context.Context, refreshToken string) (refreshResp, error) {
	refreshed, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (*tokens.RefreshResult, error) {
		return a.tokens.TxnRefreshAccessToken(txn, refreshToken, a.config.Accounts.RefreshAccessTokenExpire)
	})
	if err != nil {
		return refreshResp{}, err
	} else if refreshed == nil {
		return refreshResp{}, types.ErrUserNotFound
	}
	resp := refreshResp{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken}
	if refreshed.ExpiresAt.UnixMicro() != 0 {
		remaining := max(time.Until(refreshed.ExpiresAt).Milliseconds(), 0)
		resp.ExpiresInMS = &remaining
	}
	return resp, nil
}

func (a *AccountsDatabase) CleanupExpiredAccessTokens(ctx context.Context, limit int) (int, error) {
	// Retain expired tokens for a day so clients can receive the soft-logout hint.
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (int, error) {
		return a.tokens.TxnCleanupExpiredAuthTokens(txn, time.Now().Add(-24*time.Hour), limit)
	})
}

func (a *AccountsDatabase) ChangePassword(
	ctx context.Context,
	userDevice types.UserDevice,
	oldPassword string,
	newPasswordHash []byte,
	logoutDevices bool,
	uiaSession, method, path string,
) error {
	err := a.runAccountUpdate(ctx, userDevice.UserID, &oldPassword, func(txn fdb.Transaction) (bool, error) {
		if err := a.txnConsumeUIASession(txn, uiaSession, userDevice, method, path); err != nil {
			return false, err
		}
		a.users.TxnSetLocalUserPasswordHash(txn, userDevice.UserID.Localpart(), newPasswordHash)
		if !logoutDevices {
			return false, nil
		}
		devices, err := txn.GetRange(a.devices.RangeForUserDevices(userDevice.UserID), fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll, Limit: types.MaxVersionstampUserVersion + 2,
		}).GetSliceWithError()
		if err != nil {
			return false, err
		}
		deviceIDs := make([]id.DeviceID, 0, len(devices))
		for _, kv := range devices {
			deviceID := types.MustNewDeviceFromBytes(kv.Value).ID
			if deviceID != userDevice.DeviceID {
				deviceIDs = append(deviceIDs, deviceID)
			}
		}
		return a.txnDeleteUserDevices(txn, userDevice.UserID, deviceIDs)
	})
	if errors.Is(err, types.ErrUIASessionExpired) {
		a.cleanupExpiredUIASession(ctx, uiaSession)
	}
	return err
}
