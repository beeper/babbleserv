package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type UsersDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	profiles,
	memberships,
	membershipChanges,
	outlierMemberships subspace.Subspace

	// UserID/DeviceID/ConnID/RoomID to rooms versions (events-vstamp, receipts-vstamp)
	deviceConnRoomPositions subspace.Subspace
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
		profiles:           usersDir.Sub("pro"),
		memberships:        usersDir.Sub("mem"),
		membershipChanges:  usersDir.Sub("mch"),
		outlierMemberships: usersDir.Sub("out"),
	}
}

func (u *UsersDirectory) KeyForProfile(userID id.UserID) fdb.Key {
	return u.profiles.Pack(tuple.Tuple{userID.String()})
}

// User memberships (user_id, room_id) -> membership
//

func (u *UsersDirectory) KeyForMembership(userID id.UserID, roomID id.RoomID) fdb.Key {
	return u.memberships.Pack(tuple.Tuple{userID.String(), roomID.String()})
}

func (u *UsersDirectory) RangeForMemberships(userID id.UserID) fdb.Range {
	return u.memberships.Sub(userID.String())
}

// User membership changes (user_id, version) -> (room_id, membership)
//

func (u *UsersDirectory) KeyToMembershipChangeVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := u.membershipChanges.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (u *UsersDirectory) KeyForMembershipChange(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	key, err := u.membershipChanges.PackWithVersionstamp(tuple.Tuple{
		userID.String(), version,
	})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) RangeForMembershipChanges(
	userID id.UserID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(u.membershipChanges, fromVersion, toVersion, userID.String())
}

// User outlier memberships (user_id, room_id) -> event_id
//

func (u *UsersDirectory) KeyForOutlierMembership(userID id.UserID, roomID id.RoomID) fdb.Key {
	return u.outlierMemberships.Pack(tuple.Tuple{userID.String(), roomID.String()})
}

func (u *UsersDirectory) RangeForOutlierMemberships(userID id.UserID) fdb.Range {
	return u.outlierMemberships.Sub(userID.String())
}
