package tokens

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util"
)

type AuthTokenTup struct {
	UserID   id.UserID
	DeviceID id.DeviceID
	Expires  time.Time
}

type RefreshTokenTup struct {
	UserID   id.UserID
	DeviceID id.DeviceID
}

type TokensDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	authTokens,
	authTokensByDeviceID,
	refreshTokens,
	refreshTokensByDeviceID subspace.Subspace
}

func NewTokensDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *TokensDirectory {
	tokensDir, err := parentDir.CreateOrOpen(db, []string{"tokens"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "tokens").Logger()
	log.Debug().
		Bytes("prefix", tokensDir.Bytes()).
		Msg("Init accounts/tokens directory")

	return &TokensDirectory{
		log: log,
		db:  db,

		authTokens:              tokensDir.Sub("at"),  // token -> AuthTokenTup
		authTokensByDeviceID:    tokensDir.Sub("atd"), // userID/deviceID/token -> ''
		refreshTokens:           tokensDir.Sub("ref"), // token -> RefreshTokenTup
		refreshTokensByDeviceID: tokensDir.Sub("rfd"), // userID/deviceID/token -> ''
	}
}

func (t *TokensDirectory) TxnGetAuthTokenTup(txn fdb.ReadTransaction, token string) (*AuthTokenTup, error) {
	key := t.authTokens.Pack(tuple.Tuple{token})
	v, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	} else if v == nil {
		return nil, nil
	}
	return valueToAuthTokenTup(v), nil
}

func (t *TokensDirectory) TxnCreateAuthToken(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	expires time.Duration,
) string {
	token := util.GenerateRandomString(48)

	var expireTs int64
	if expires > 0 {
		expireTs = time.Now().UTC().Add(expires).UnixMicro()
	}

	dKey := t.authTokensByDeviceID.Pack(tuple.Tuple{userID.String(), deviceID.String(), token})
	txn.Set(dKey, nil)

	key := t.authTokens.Pack(tuple.Tuple{token})
	value := tuple.Tuple{userID.String(), deviceID.String(), expireTs}.Pack()
	txn.Set(key, value)

	return token
}

func (t *TokensDirectory) TxnCreateRefreshToken(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
) string {
	token := util.GenerateRandomString(48)

	dKey := t.refreshTokensByDeviceID.Pack(tuple.Tuple{userID.String(), deviceID.String(), token})
	txn.Set(dKey, nil)

	key := t.refreshTokens.Pack(tuple.Tuple{token})
	value := tuple.Tuple{userID.String(), deviceID.String()}.Pack()
	txn.Set(key, value)

	return token
}

func (t *TokensDirectory) TxnCreateNewTokensForUserDevice(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	withRefreshToken bool,
	accessTokenExpire time.Duration,
) (string, string) {
	t.TxnClearUserDeviceTokens(txn, userID, deviceID)

	var expire time.Duration
	var refreshToken string

	if withRefreshToken {
		expire = accessTokenExpire
		refreshToken = t.TxnCreateRefreshToken(txn, userID, deviceID)
	}

	accessToken := t.TxnCreateAuthToken(txn, userID, deviceID, expire)

	return accessToken, refreshToken
}

func (t *TokensDirectory) TxnClearUserDeviceTokens(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
) error {
	if err := util.TxnIterAllRange(txn, t.refreshTokensByDeviceID.Sub(userID.String(), deviceID.String()), func(kv fdb.KeyValue) error {
		txn.Clear(t.refreshTokens.Pack(tuple.Tuple{kv.Value}))
		txn.Clear(kv.Key)
		return nil
	}); err != nil {
		return err
	}

	if err := util.TxnIterAllRange(txn, t.authTokensByDeviceID.Sub(userID.String(), deviceID.String()), func(kv fdb.KeyValue) error {
		txn.Clear(t.authTokens.Pack(tuple.Tuple{kv.Value}))
		txn.Clear(kv.Key)
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (t *TokensDirectory) TxnListUserDeviceAuthTokenPrefixes(
	txn fdb.ReadTransaction,
	userID id.UserID,
) (map[id.DeviceID][]string, error) {
	tokens := make(map[id.DeviceID][]string, 5)

	if err := util.TxnIterAllRange(txn, t.authTokensByDeviceID.Sub(userID.String()), func(kv fdb.KeyValue) error {
		tup, _ := t.authTokensByDeviceID.Unpack(kv.Key)
		deviceID := id.DeviceID(tup[1].(string))
		tokenPrefix := tup[2].(string)[:7] + "..."
		tokens[deviceID] = append(tokens[deviceID], tokenPrefix)
		return nil
	}); err != nil {
		return nil, err
	} else {
		return tokens, nil
	}
}

func (t *TokensDirectory) TxnListUserDeviceRefreshTokenPrefixes(
	txn fdb.ReadTransaction,
	userID id.UserID,
) (map[id.DeviceID][]string, error) {
	tokens := make(map[id.DeviceID][]string, 5)

	if err := util.TxnIterAllRange(txn, t.refreshTokensByDeviceID.Sub(userID.String()), func(kv fdb.KeyValue) error {
		tup, _ := t.refreshTokensByDeviceID.Unpack(kv.Key)
		deviceID := id.DeviceID(tup[1].(string))
		tokenPrefix := tup[2].(string)[:7] + "..."
		tokens[deviceID] = append(tokens[deviceID], tokenPrefix)
		return nil
	}); err != nil {
		return nil, err
	} else {
		return tokens, nil
	}
}

func valueToAuthTokenTup(v []byte) *AuthTokenTup {
	tup, _ := tuple.Unpack(v)
	return &AuthTokenTup{
		UserID:   id.UserID(tup[0].(string)),
		DeviceID: id.DeviceID(tup[1].(string)),
		Expires:  time.UnixMicro(tup[2].(int64)),
	}
}

func valueToRefreshTokenTup(v []byte) *RefreshTokenTup {
	tup, _ := tuple.Unpack(v)
	return &RefreshTokenTup{
		UserID:   id.UserID(tup[0].(string)),
		DeviceID: id.DeviceID(tup[1].(string)),
	}
}
