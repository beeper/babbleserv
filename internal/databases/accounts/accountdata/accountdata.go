package accountdata

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type AccountDataDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// AccountDataTup to version, used to clear previous keys when new ones appended
	//
	// key: (UserID, RoomID, Type)
	// value: Versionstamp
	accountDataTupToVersion subspace.Subspace

	// Per user sparse stream of account data, only contains the latest version of each tup
	//
	// - get whole range for init sync (all account data)
	// - paginate range for inc sync
	//
	// key: (UserID, Versionstamp)
	// value: types.AccountData
	userVersionToAccountData subspace.Subspace
}

func NewAccountDataDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *AccountDataDirectory {
	accountDataDir, err := parentDir.CreateOrOpen(db, []string{"accountdata"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "accountdata").Logger()
	log.Debug().
		Bytes("prefix", accountDataDir.Bytes()).
		Msg("Init accounts/accountdata directory")

	return &AccountDataDirectory{
		log: log,
		db:  db,

		accountDataTupToVersion:  accountDataDir.Sub("atv"),
		userVersionToAccountData: accountDataDir.Sub("uva"),
	}
}

func (a *AccountDataDirectory) KeyForAccountDataVersion(tup types.AccountDataTup) fdb.Key {
	return a.accountDataTupToVersion.Pack(tuple.Tuple{
		tup.UserID.String(),
		tup.RoomID.String(),
		tup.Type.String(),
	})
}

func (a *AccountDataDirectory) KeyForUserVersion(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	tup := tuple.Tuple{userID.String(), version}
	if types.IsIncompleteVersionstamp(version) {
		key, err := a.userVersionToAccountData.PackWithVersionstamp(tup)
		if err != nil {
			panic(err)
		}
		return key
	}
	return a.userVersionToAccountData.Pack(tup)
}

func (a *AccountDataDirectory) RangeForUserVersion(
	userID id.UserID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(a.userVersionToAccountData, fromVersion, toVersion, userID.String())
}
