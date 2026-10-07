package users

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type UsersDirectory struct {
	log        zerolog.Logger
	db         fdb.Database
	serverName string

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

	// User push notification endpoints (pushers)
	//
	// key: (id.UserID, appID, pushKey)
	// value: pushgateway.Pusher (JSON)
	userPushers subspace.Subspace

	// key: (id.UserID, appID, pushKey)
	// value: id.DeviceID which last set the pusher
	userPusherDevices subspace.Subspace

	// key: (appID, pushKey, id.UserID)
	// value: empty; finds owners of exactly one pusher identity
	pusherUsersByIdentity subspace.Subspace

	// Lowercase one-to-three-rune substring -> user ID; value: empty.
	searchGrams subspace.Subspace

	// user ID -> (source membership event ID, due milliseconds)
	remoteProfileJobs subspace.Subspace
	// (due milliseconds, user ID, source membership event ID) -> empty
	remoteProfileJobsByDue subspace.Subspace
}

func NewUsersDirectory(
	logger zerolog.Logger,
	db fdb.Database,
	parentDir directory.Directory,
	serverName string,
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
		log:        log,
		db:         db,
		serverName: serverName,

		byVersion:              usersDir.Sub("uvr"),
		localUsers:             usersDir.Sub("unm"),
		remoteUsers:            usersDir.Sub("rus"),
		userProfiles:           usersDir.Sub("upr"),
		profileChanges:         usersDir.Sub("pch"),
		userPasswordHashes:     usersDir.Sub("uph"),
		userCrossSigningKeys:   usersDir.Sub("uxs"),
		userKeySignatures:      usersDir.Sub("uks"),
		userFilters:            usersDir.Sub("ufl"),
		userPushers:            usersDir.Sub("upk"),
		userPusherDevices:      usersDir.Sub("upd"),
		pusherUsersByIdentity:  usersDir.Sub("upi"),
		searchGrams:            usersDir.Sub("usg"),
		remoteProfileJobs:      usersDir.Sub("urj"),
		remoteProfileJobsByDue: usersDir.Sub("urd"),
	}
}

func (u *UsersDirectory) TxnGetLocalUserPasswordHash(txn fdb.ReadTransaction, username string) ([]byte, error) {
	key := u.userPasswordHashes.Pack(tuple.Tuple{username})
	return txn.Get(key).Get()
}

func (u *UsersDirectory) TxnSetLocalUserPasswordHash(txn fdb.Transaction, username string, hash []byte) {
	txn.Set(u.userPasswordHashes.Pack(tuple.Tuple{username}), hash)
}

func (u *UsersDirectory) keyForUser(userID id.UserID) fdb.Key {
	if userID.Homeserver() == u.serverName {
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
	if userID.Homeserver() != u.serverName {
		return nil, fmt.Errorf("userid is not local: %s", userID)
	}
	return u.txnGetUser(txn, userID)
}

func (u *UsersDirectory) TxnGetRemoteUser(txn fdb.ReadTransaction, userID id.UserID) (*types.User, error) {
	if userID.Homeserver() == u.serverName {
		return nil, fmt.Errorf("userid is not remote: %s", userID)
	}
	return u.txnGetUser(txn, userID)
}

func (u *UsersDirectory) TxnGetLocalUserFuture(txn fdb.ReadTransaction, userID id.UserID) (func() (*types.User, error), error) {
	if userID.Homeserver() != u.serverName {
		return nil, fmt.Errorf("userid is not local: %s", userID)
	}
	return u.txnGetUserFuture(txn, userID), nil
}

func (u *UsersDirectory) txnGetUser(txn fdb.ReadTransaction, userID id.UserID) (*types.User, error) {
	return u.txnGetUserFuture(txn, userID)()
}

func (u *UsersDirectory) txnGetUserFuture(txn fdb.ReadTransaction, userID id.UserID) func() (*types.User, error) {
	future := txn.Get(u.keyForUser(userID))
	return func() (*types.User, error) {
		b, err := future.Get()
		if err != nil || b == nil {
			return nil, err
		}
		return types.NewUserFromBytes(b, userID.Localpart(), userID.Homeserver())
	}
}

func (u *UsersDirectory) TxnCreateLocalUser(txn fdb.Transaction, user *types.User, hashedPassword []byte) error {
	userID := user.UserID()

	if userID.Homeserver() != u.serverName {
		return fmt.Errorf("userid is not local: %s", userID)
	}

	if err := u.txnCreateUser(txn, user); err != nil {
		return err
	}

	for gram := range searchGrams(userID.String()) {
		txn.Set(u.searchGrams.Pack(tuple.Tuple{gram, userID.String()}), nil)
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

func (u *UsersDirectory) TxnAllocateDeviceListVersion(txn fdb.Transaction, userID id.UserID) (types.DeviceListStream, error) {
	user, err := u.TxnGetLocalUser(txn, userID)
	if err != nil {
		return types.DeviceListStream{}, err
	} else if user == nil {
		return types.DeviceListStream{}, fmt.Errorf("%w: %s", types.ErrUserNotFound, userID)
	}

	stream := types.DeviceListStream{
		StreamID: user.DeviceListVersion + 1,
		PrevID:   user.DeviceListVersion,
	}
	user.DeviceListVersion = stream.StreamID
	txn.Set(u.keyForUser(userID), user.ToMsgpack())
	return stream, nil
}
