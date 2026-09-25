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

type UIASessionTup struct {
	UserID    id.UserID
	DeviceID  id.DeviceID
	Method    string
	Path      string
	Request   []byte
	ExpiresAt time.Time
}

type TokensDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	authTokens,
	authTokensByDeviceID,
	refreshTokens,
	refreshTokensByDeviceID,
	uiaSessions,
	uiaSessionsByExpiry,
	uiaSessionsByDeviceID subspace.Subspace
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
		uiaSessions:             tokensDir.Sub("uia"), // session -> UIASessionTup
		uiaSessionsByExpiry:     tokensDir.Sub("uie"), // expiresAt/session -> empty
		uiaSessionsByDeviceID:   tokensDir.Sub("uid"), // userID/deviceID/session -> empty
		refreshTokens:           tokensDir.Sub("ref"), // token -> RefreshTokenTup
		refreshTokensByDeviceID: tokensDir.Sub("rfd"), // userID/deviceID/token -> ''
	}
}

func (t *TokensDirectory) TxnCreateUIASession(
	txn fdb.Transaction,
	session string,
	userID id.UserID,
	deviceID id.DeviceID,
	method, path string,
	request []byte,
	expiresAt time.Time,
) {
	txn.Set(t.uiaSessions.Pack(tuple.Tuple{session}), tuple.Tuple{
		userID.String(), deviceID.String(), method, path, request, expiresAt.UnixMicro(),
	}.Pack())
	txn.Set(t.uiaSessionsByExpiry.Pack(tuple.Tuple{expiresAt.UnixMicro(), session}), nil)
	txn.Set(t.uiaSessionsByDeviceID.Pack(tuple.Tuple{userID.String(), deviceID.String(), session}), nil)
}

func (t *TokensDirectory) TxnGetUIASession(txn fdb.ReadTransaction, session string) (*UIASessionTup, error) {
	value, err := txn.Get(t.uiaSessions.Pack(tuple.Tuple{session})).Get()
	if err != nil || value == nil {
		return nil, err
	}
	tup, err := tuple.Unpack(value)
	if err != nil {
		return nil, err
	}
	return &UIASessionTup{
		UserID:    id.UserID(tup[0].(string)),
		DeviceID:  id.DeviceID(tup[1].(string)),
		Method:    tup[2].(string),
		Path:      tup[3].(string),
		Request:   tup[4].([]byte),
		ExpiresAt: time.UnixMicro(tup[5].(int64)),
	}, nil
}

func (t *TokensDirectory) txnDeleteUIASession(
	txn fdb.Transaction,
	session string,
	uia *UIASessionTup,
) {
	txn.Clear(t.uiaSessions.Pack(tuple.Tuple{session}))
	if uia != nil {
		txn.Clear(t.uiaSessionsByExpiry.Pack(tuple.Tuple{uia.ExpiresAt.UnixMicro(), session}))
		txn.Clear(t.uiaSessionsByDeviceID.Pack(tuple.Tuple{
			uia.UserID.String(), uia.DeviceID.String(), session,
		}))
	}
}

func (t *TokensDirectory) TxnDeleteUIASession(txn fdb.Transaction, session string) error {
	uia, err := t.TxnGetUIASession(txn, session)
	if err != nil {
		return err
	}
	t.txnDeleteUIASession(txn, session, uia)
	return nil
}

func (t *TokensDirectory) TxnClearDeviceUIASessions(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
) error {
	return util.TxnIterAllRange(txn, t.uiaSessionsByDeviceID.Sub(userID.String(), deviceID.String()), func(kv fdb.KeyValue) error {
		tup, err := t.uiaSessionsByDeviceID.Unpack(kv.Key)
		if err != nil {
			return err
		}
		session := tup[2].(string)
		uia, err := t.TxnGetUIASession(txn, session)
		if err != nil {
			return err
		}
		if uia == nil {
			txn.Clear(kv.Key)
			return nil
		}
		t.txnDeleteUIASession(txn, session, uia)
		return nil
	})
}

// TxnCleanupExpiredUIASessions removes at most limit expiry-index rows and their primary/owner
// records. The expiry index keeps cleanup work bounded and avoids scanning live sessions.
func (t *TokensDirectory) TxnCleanupExpiredUIASessions(
	txn fdb.Transaction,
	now time.Time,
	limit int,
) (int, error) {
	rng := fdb.KeyRange{
		Begin: fdb.Key(t.uiaSessionsByExpiry.Bytes()),
		End:   t.uiaSessionsByExpiry.Pack(tuple.Tuple{now.UnixMicro() + 1}),
	}
	iter := txn.GetRange(rng, fdb.RangeOptions{
		Limit: limit,
		Mode:  fdb.StreamingModeIterator,
	}).Iterator()
	deleted := 0
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return deleted, err
		}
		tup, err := t.uiaSessionsByExpiry.Unpack(kv.Key)
		if err != nil {
			return deleted, err
		}
		session := tup[1].(string)
		uia, err := t.TxnGetUIASession(txn, session)
		if err != nil {
			return deleted, err
		}
		if uia == nil {
			txn.Clear(kv.Key)
		} else {
			t.txnDeleteUIASession(txn, session, uia)
		}
		deleted++
	}
	return deleted, nil
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
	if err := t.TxnClearUserDeviceTokens(txn, userID, deviceID); err != nil {
		panic(err)
	}
	if err := t.TxnClearDeviceUIASessions(txn, userID, deviceID); err != nil {
		panic(err)
	}

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
		tup, err := t.refreshTokensByDeviceID.Unpack(kv.Key)
		if err != nil {
			return err
		}
		txn.Clear(t.refreshTokens.Pack(tuple.Tuple{tup[2].(string)}))
		txn.Clear(kv.Key)
		return nil
	}); err != nil {
		return err
	}

	if err := util.TxnIterAllRange(txn, t.authTokensByDeviceID.Sub(userID.String(), deviceID.String()), func(kv fdb.KeyValue) error {
		tup, err := t.authTokensByDeviceID.Unpack(kv.Key)
		if err != nil {
			return err
		}
		txn.Clear(t.authTokens.Pack(tuple.Tuple{tup[2].(string)}))
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
