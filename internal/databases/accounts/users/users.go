package users

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/types"
)

type UsersDirectory struct {
	log    zerolog.Logger
	db     fdb.Database
	config config.BabbleConfig

	/// version -> user index
	//
	// key: tuple.Versionstamp
	// value: id.UserID
	byVersion subspace.Subspace

	/// userID -> types.User for local users (this homeserver)
	//
	// key: id.UserID
	// value: types.User
	localUsers subspace.Subspace

	/// userID -> types.User for remote users (other homeservers)
	//
	// key: id.UserID
	// value: types.User
	remoteUsers subspace.Subspace

	// user -> types.UserProfile
	//
	// key: id.UserID
	// value: types.UserProfile
	userProfiles subspace.Subspace

	// version -> profile, paginated and cleared by the ProfileChangeIterator worker
	//
	// key: tuple.Versionstamp
	// value: (id.UserID, types.UserProfile)
	profileChanges subspace.Subspace

	// Cross signing signing keys
	// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3keysdevice_signingupload
	// user -> CrossSigningKey JSON (CSAPI)
	//
	// key: id.UserID
	// value: types.userCrossSigningKeys
	userCrossSigningKeys subspace.Subspace

	// Key signatures
	// Stored under signing user prefix, with target user/key next so we can fetch signatures
	// from user X for user Ys xs/device keys. Last part of the key is the signing users keyid
	// such that we have a unique user/key combo signing another user/key.
	//
	// key: (id.UserID (mine), id.UserID (other), KeyID (other), KeyID (mine))
	// value: []byte
	userKeySignatures subspace.Subspace

	// Local user only data, keyed by username not userID and methods must refuse nonlocal userIDs
	//

	// username -> password hash
	//
	// key: username
	// value: []byte
	userPasswordHashes subspace.Subspace

	// Username/version -> filter bytes, version returned to the user as the filter ID. PUT requests
	// will then update the filter in-place. Meaning filters aren't stored as an immutableish stream.
	//
	// key: (Username, Versionstamp)
	// value: matruix.Filter
	userFilters subspace.Subspace
}

func NewUsersDirectory(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db fdb.Database,
	parentDir directory.Directory,
) *UsersDirectory {
	usersDir, err := parentDir.CreateOrOpen(db, []string{"users"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "users").Logger()
	log.Debug().
		Bytes("prefix", usersDir.Bytes()).
		Msg("Init accounts/users directory")

	return &UsersDirectory{
		log:    log,
		db:     db,
		config: cfg,

		byVersion:            usersDir.Sub("uvr"),
		localUsers:           usersDir.Sub("unm"),
		remoteUsers:          usersDir.Sub("rus"),
		userProfiles:         usersDir.Sub("upr"),
		profileChanges:       usersDir.Sub("pch"),
		userPasswordHashes:   usersDir.Sub("uph"),
		userCrossSigningKeys: usersDir.Sub("uxs"),
		userKeySignatures:    usersDir.Sub("uks"),
		userFilters:          usersDir.Sub("ufl"),
	}
}

func (u *UsersDirectory) TxnGetLocalUserPasswordHash(txn fdb.ReadTransaction, username string) ([]byte, error) {
	key := u.userPasswordHashes.Pack(tuple.Tuple{username})
	return txn.Get(key).Get()
}

func (u *UsersDirectory) keyForUser(userID id.UserID) fdb.Key {
	if userID.Homeserver() == u.config.ServerName {
		return u.localUsers.Pack(tuple.Tuple{userID.String()})
	}
	return u.remoteUsers.Pack(tuple.Tuple{userID.String()})
}

func (u *UsersDirectory) keyForUserVersion(version tuple.Versionstamp) fdb.Key {
	key, err := u.byVersion.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) TxnGetLocalUser(txn fdb.ReadTransaction, userID id.UserID) (*types.User, error) {
	if userID.Homeserver() != u.config.ServerName {
		return nil, fmt.Errorf("userid is not local: %s", userID)
	}
	return u.txnGetUser(txn, userID)
}

func (u *UsersDirectory) TxnGetRemoteUser(txn fdb.ReadTransaction, userID id.UserID) (*types.User, error) {
	if userID.Homeserver() == u.config.ServerName {
		return nil, fmt.Errorf("userid is not remote: %s", userID)
	}
	return u.txnGetUser(txn, userID)
}

func (u *UsersDirectory) txnGetUser(txn fdb.ReadTransaction, userID id.UserID) (*types.User, error) {
	b, err := txn.Get(u.keyForUser(userID)).Get()
	if err != nil {
		return nil, err
	} else if b == nil {
		return nil, nil
	}
	return types.NewUserFromBytes(b, userID.Localpart(), userID.Homeserver())
}

func (u *UsersDirectory) TxnCreateLocalUser(txn fdb.Transaction, user *types.User, hashedPassword []byte) error {
	userID := user.UserID()

	if userID.Homeserver() != u.config.ServerName {
		return fmt.Errorf("userid is not local: %s", userID)
	}

	if err := u.txnCreateUser(txn, user); err != nil {
		return err
	}

	if hashedPassword != nil {
		txn.Set(u.userPasswordHashes.Pack(tuple.Tuple{user.Username}), hashedPassword)
	}

	return nil
}

func (u *UsersDirectory) txnCreateUser(txn fdb.Transaction, user *types.User) error {
	userID := user.UserID()

	key := u.keyForUser(userID)

	existing, err := txn.Get(key).Get()
	if err != nil {
		return err
	} else if existing != nil {
		return types.ErrUserAlreadyExists
	}

	txn.Set(key, user.ToMsgpack())

	// version -> userID
	txn.SetVersionstampedKey(u.keyForUserVersion(tuple.IncompleteVersionstamp(0)), []byte(userID))
	return nil
}

func (u *UsersDirectory) TxnIncrementUserDeviceListVersion(txn fdb.Transaction, userID id.UserID) error {
	user, err := u.txnGetUser(txn, userID)
	if err != nil {
		return nil
	} else if user == nil {
		if userID.Homeserver() == u.config.ServerName {
			return fmt.Errorf("user not found for local userid: %s", userID)
		}

		// We lazily create remote users to track their device list versions
		user = &types.User{
			Username:   userID.Localpart(),
			ServerName: userID.Homeserver(),
		}
		if err := u.txnCreateUser(txn, user); err != nil {
			return err
		}
	}

	user.DeviceListVersion += 1
	txn.Set(u.keyForUser(userID), user.ToMsgpack())
	return nil
}
