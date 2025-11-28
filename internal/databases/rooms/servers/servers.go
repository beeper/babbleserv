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

	// UserIDs by room/server so we can easily detect when a server goes in/out of a room
	//
	// key: (RoomID, ServerName, UserID)
	// value: []byte (always empty)
	joinedRoomMembers subspace.Subspace

	// Room memberships by server so we list (joined) rooms for a given server
	//
	// key: (ServerName, RoomID)
	// value: []byte (always empty)
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

		joinedRoomMembers: serversDir.Sub("jme"),
		memberships:       serversDir.Sub("mem"),
		membershipChanges: serversDir.Sub("mch"),
	}
}

// Server joined members (room_id, server_name, username) -> ''
//

func (s *ServersDirectory) KeyForRoomJoinedMember(roomID id.RoomID, serverName string, username string) fdb.Key {
	return s.joinedRoomMembers.Pack(tuple.Tuple{roomID.String(), serverName, username})
}

func (s *ServersDirectory) RangeForRoomJoinedMembers(roomID id.RoomID, serverName string) fdb.Range {
	return s.joinedRoomMembers.Sub(roomID.String(), serverName)
}

// Server memberships (server_name, room_id) -> '' (we only care about join)
//

func (s *ServersDirectory) KeyForMembership(serverName string, roomID id.RoomID) fdb.Key {
	return s.memberships.Pack(tuple.Tuple{serverName, roomID.String()})
}

func (s *ServersDirectory) RangeForMemberships(serverName string) fdb.Range {
	return s.memberships.Sub(serverName)
}

// Server membership changes (server_name, version) -> (room_id, membership)
//

func (s *ServersDirectory) KeyToMembershipChangeVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := s.membershipChanges.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (s *ServersDirectory) KeyForMembershipChange(serverName string, version tuple.Versionstamp) fdb.Key {
	key, err := s.membershipChanges.PackWithVersionstamp(tuple.Tuple{
		serverName, version,
	})
	if err != nil {
		panic(err)
	}
	return key
}

func (s *ServersDirectory) RangeForMembershipChanges(
	serverName string,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(s.membershipChanges, fromVersion, toVersion, serverName)
}
