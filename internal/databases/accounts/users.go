package accounts

import (
	"bytes"
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	"maunium.net/go/mautrix/id"

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

		if _, err = a.devices.TxnGetOrCreateDevice(txn, userID, deviceID, initialDeviceDisplayName); err != nil {
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
func (a *AccountsDatabase) RegisterWithPassword(
	ctx context.Context,
	username string,
	password []byte,
	withRefreshToken bool,
	deviceID id.DeviceID,
	initialDeviceDisplayName string,
) (authResp, error) {
	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	resp := authResp{
		DeviceID: deviceID,
	}

	hashedPassword, err := bcrypt.GenerateFromPassword(password, 12)
	if err != nil {
		return resp, err
	}

	if _, err = util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (*struct{}, error) {
		user := types.User{
			Username:   username,
			ServerName: a.config.ServerName,
			CreatedAt:  time.Now().UTC(),
		}

		if err := a.users.TxnCreateLocalUser(txn, &user, hashedPassword); err != nil {
			return nil, err
		}

		userID := user.UserID()
		resp.UserID = userID

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

	a.notifier.SendChange(notifier.Change{
		UserIDs: []id.UserID{resp.UserID},
	})
	zerolog.Ctx(ctx).
		Info().
		Str("username", username).
		Str("device_id", deviceID.String()).
		Msg("Registered new user")

	return resp, nil
}

func (a *AccountsDatabase) runDeviceRemoval(
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
