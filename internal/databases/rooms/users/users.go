package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"
)

type UsersDirectory struct {
	log zerolog.Logger
	db  fdb.Database
	ByID,
	Memberships,
	MembershipChanges subspace.Subspace
}

func NewUsersDirectory(logger zerolog.Logger, db fdb.Database) *UsersDirectory {
	usersDir, err := directory.CreateOrOpen(db, []string{"usr"}, nil)
	if err != nil {
		panic(err)
	}

	return &UsersDirectory{
		log: logger.With().Str("database", "rmusers").Logger(),
		db:  db,

		// Init data model subspaces, subspace prefixes are intentionally short
		// "When using the tuple layer to encode keys (as is recommended), select short strings or small integers for tuple elements."
		// https://apple.github.io/foundationdb/data-modeling.html#key-and-value-sizes
		ByID:              usersDir.Sub("id"),
		Memberships:       usersDir.Sub("mem"),
		MembershipChanges: usersDir.Sub("mch"),
	}
}

func (u *UsersDirectory) KeyForUser(userID id.UserID) fdb.Key {
	return u.ByID.Pack(tuple.Tuple{userID.String()})
}

func (u *UsersDirectory) KeyForUserMembership(userID id.UserID, roomID id.RoomID) fdb.Key {
	return u.Memberships.Pack(tuple.Tuple{userID.String(), roomID.String()})
}
func (u *UsersDirectory) RangeForUserMemberships(userID id.UserID) fdb.Range {
	return u.Memberships.Sub(userID.String())
}

func (u *UsersDirectory) KeyForUserMembershipVersion(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	key, err := u.MembershipChanges.PackWithVersionstamp(tuple.Tuple{
		userID.String(), version,
	})
	if err != nil {
		panic(err)
	}
	return key
}
