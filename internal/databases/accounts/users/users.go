package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/types"
)

type UsersDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	/// version -> username
	byVersion subspace.Subspace

	/// username -> *types.User
	//
	// key: (Username)
	// value: types.User (as msgpack []byte)
	byUsername subspace.Subspace

	// username -> password hash
	userPasswordHashes subspace.Subspace

	// Device signing keys
	// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3keysdevice_signingupload
	// username -> CrossSigningKey JSON (CSAPI)
	userMasterKeys subspace.Subspace

	// username -> CrossSigningKey JSON (CSAPI)
	userSelfSigningKeys subspace.Subspace

	// username -> CrossSigningKey JSON (CSAPI)
	userUserSigningKeys subspace.Subspace

	// Username/version -> filter bytes, version returned to the user as the filter ID. PUT requests
	// will then update the filter in-place. Meaning filters aren't stored as an immutableish stream.
	userFilters subspace.Subspace
}

func NewUsersDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *UsersDirectory {
	usersDir, err := parentDir.CreateOrOpen(db, []string{"users"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "users").Logger()
	log.Debug().
		Bytes("prefix", usersDir.Bytes()).
		Msg("Init accounts/users directory")

	return &UsersDirectory{
		log: log,
		db:  db,

		byVersion:           usersDir.Sub("ver"),
		byUsername:          usersDir.Sub("unm"),
		userPasswordHashes:  usersDir.Sub("uph"),
		userMasterKeys:      usersDir.Sub("key"),
		userSelfSigningKeys: usersDir.Sub("ssk"),
		userUserSigningKeys: usersDir.Sub("usk"),
		userFilters:         usersDir.Sub("ufl"),
	}
}

func (u *UsersDirectory) TxnGetLocalUserPasswordHash(txn fdb.ReadTransaction, username string) ([]byte, error) {
	key := u.userPasswordHashes.Pack(tuple.Tuple{username})
	return txn.Get(key).Get()
}

func (u *UsersDirectory) TxnSetLocalUserPasswordHash(txn fdb.Transaction, username string, hash []byte) {
	key := u.userPasswordHashes.Pack(tuple.Tuple{username})
	txn.Set(key, hash)
}

func (u *UsersDirectory) TxnGetLocalUser(txn fdb.ReadTransaction, username, serverName string) (*types.User, error) {
	key := u.keyForUser(username)

	b, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	} else if b == nil {
		return nil, nil
	}

	return types.NewUserFromBytes(b, username, serverName)
}

func (u *UsersDirectory) TxnCreateUser(txn fdb.Transaction, user *types.User, hashedPassword []byte) error {
	key := u.keyForUser(user.Username)

	existing, err := txn.Get(key).Get()
	if err != nil {
		return err
	} else if existing != nil {
		return types.ErrUserAlreadyExists
	}

	txn.Set(key, user.ToMsgpack())

	// version -> username
	key = u.keyForUserVersion(tuple.IncompleteVersionstamp(0))
	txn.Set(key, []byte(user.Username))

	if hashedPassword != nil {
		u.TxnSetLocalUserPasswordHash(txn, user.Username, hashedPassword)
	}

	return nil
}

func (u *UsersDirectory) keyForUser(username string) fdb.Key {
	return u.byUsername.Pack(tuple.Tuple{username})
}

func (u *UsersDirectory) keyForUserVersion(version tuple.Versionstamp) fdb.Key {
	key, err := u.byVersion.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) KeyForNewUserFilter(username string, version tuple.Versionstamp) fdb.Key {
	key, err := u.userFilters.PackWithVersionstamp(tuple.Tuple{username, version})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) KeyForUserFilter(username string, version tuple.Versionstamp) fdb.Key {
	return u.userFilters.Pack(tuple.Tuple{username, version})
}
