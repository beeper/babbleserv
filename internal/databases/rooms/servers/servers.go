package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type ServersDirectory struct {
	log zerolog.Logger

	// Joined member counts by room/server, absent at zero: whether a server is in a room, and the
	// room's servers.
	//
	// key: (RoomID, ServerName)
	// value: (count)
	joinedCounts subspace.Subspace

	// Room memberships by server so we list (joined) rooms for a given server
	//
	// key: (ServerName, RoomID)
	// value: types.MembershipTup
	memberships subspace.Subspace

	// Room membership changes by server so we can handle changes during sync (for federation)
	//
	// key: (ServerName, Versionstamp)
	// value: types.MembershipTup
	membershipChanges subspace.Subspace
}

func NewServersDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ServersDirectory {
	serversDir, err := parentDir.CreateOrOpen(db, []string{"servers"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "servers").Logger()
	log.Debug().
		Bytes("prefix", serversDir.Bytes()).
		Msg("Init rooms/servers directory")

	return &ServersDirectory{
		log: log,

		joinedCounts:      serversDir.Sub("jcn"),
		memberships:       serversDir.Sub("mem"),
		membershipChanges: serversDir.Sub("mch"),
	}
}

// Server joined member counts (room_id, server_name) -> (count)
//

func (s *ServersDirectory) KeyForJoinedCount(roomID id.RoomID, serverName string) fdb.Key {
	return s.joinedCounts.Pack(tuple.Tuple{roomID.String(), serverName})
}

func (s *ServersDirectory) rangeForJoinedCounts(roomID id.RoomID) fdb.Range {
	return s.joinedCounts.Sub(roomID.String())
}

// Server memberships (server_name, room_id) -> MembershipTup, set only while joined
//

func (s *ServersDirectory) KeyForMembership(serverName string, roomID id.RoomID) fdb.Key {
	return s.memberships.Pack(tuple.Tuple{serverName, roomID.String()})
}

func (s *ServersDirectory) rangeForMemberships(serverName string) fdb.Range {
	return s.memberships.Sub(serverName)
}

// Server membership changes (server_name, version) -> MembershipTup
//

func (s *ServersDirectory) keyToMembershipChangeVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := s.membershipChanges.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (s *ServersDirectory) KeyForMembershipChange(serverName string, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(s.membershipChanges, tuple.Tuple{serverName, version})
}

func (s *ServersDirectory) rangeForMembershipChanges(
	serverName string,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(s.membershipChanges, fromVersion, toVersion, serverName)
}
