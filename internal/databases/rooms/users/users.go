package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/rs/zerolog"
)

type UsersDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// Current user memberships
	//
	// key: (id.UserID, id.RoomID)
	// value: types.MembershipTup
	memberships subspace.Subspace

	// Membership changes
	//
	// key: (id.UserID, tuple.Versionstamp)
	// value: types.MembershipTupWithVersion
	membershipChanges subspace.Subspace

	// Notification counts per event version
	// Stored as deltas, summed for total counts, cleared on receipt
	//
	// key: (id.UserID, id.RoomID, tuple.Versionstamp)
	// value: types.Notifications (msgpack)
	notificationVersions subspace.Subspace
}

func NewUsersDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *UsersDirectory {
	usersDir, err := parentDir.CreateOrOpen(db, []string{"users"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "users").Logger()
	log.Debug().
		Bytes("prefix", usersDir.Bytes()).
		Msg("Init rooms/users directory")

	return &UsersDirectory{
		log: log,
		db:  db,

		// Init data model subspaces, subspace prefixes are intentionally short
		// "When using the tuple layer to encode keys (as is recommended), select short strings or small integers for tuple elements."
		// https://apple.github.io/foundationdb/data-modeling.html#key-and-value-sizes
		memberships:          usersDir.Sub("mem"),
		membershipChanges:    usersDir.Sub("mch"),
		notificationVersions: usersDir.Sub("nv"),
	}
}
