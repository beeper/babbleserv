package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"
)

type ServersDirectory struct {
	log zerolog.Logger

	byID,
	joinedMembers,
	memberships,
	membershipChanges,
	idToPosition subspace.Subspace
}

func NewServersDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ServersDirectory {
	serversDir, err := parentDir.CreateOrOpen(db, []string{"servers"}, nil)
	if err != nil {
		panic(err)
	}

	return &ServersDirectory{
		log: logger.With().Str("directory", "servers").Logger(),

		// Init data model subspaces, subspace prefixes are intentionally short
		// "When using the tuple layer to encode keys (as is recommended), select short strings or small integers for tuple elements."
		// https://apple.github.io/foundationdb/data-modeling.html#key-and-value-sizes
		byID: serversDir.Sub("id"),

		joinedMembers:     serversDir.Sub("jme"),
		memberships:       serversDir.Sub("mem"),
		membershipChanges: serversDir.Sub("mch"),

		idToPosition: serversDir.Sub("itt"),
	}
}

func (s *ServersDirectory) KeyForServer(serverName string) fdb.Key {
	return s.byID.Pack(tuple.Tuple{serverName})
}

func (s *ServersDirectory) KeyForServerPosition(serverName string) fdb.Key {
	return s.idToPosition.Pack(tuple.Tuple{serverName})
}

// Server joined members (room_id, server_name, username) -> ''
//

func (s *ServersDirectory) KeyForServerJoinedMember(roomID id.RoomID, serverName string, username string) fdb.Key {
	return s.joinedMembers.Pack(tuple.Tuple{roomID.String(), serverName, username})
}

func (s *ServersDirectory) RangeForServerJoinedMembers(roomID id.RoomID, serverName string) fdb.Range {
	return s.joinedMembers.Sub(roomID.String(), serverName)
}

// Server memberships (server_name, room_id) -> '' (we only care about join)
//

func (s *ServersDirectory) KeyForServerMembership(serverName string, roomID id.RoomID) fdb.Key {
	return s.memberships.Pack(tuple.Tuple{serverName, roomID.String()})
}

func (s *ServersDirectory) RangeForServerMemberships(serverName string) fdb.Range {
	return s.memberships.Sub(serverName)
}

// Server membership changes (server_name, version) -> (room_id, membership)
//

func (s *ServersDirectory) KeyForServerMembershipChange(serverName string, version tuple.Versionstamp) fdb.Key {
	key, err := s.membershipChanges.PackWithVersionstamp(tuple.Tuple{
		serverName, version,
	})
	if err != nil {
		panic(err)
	}
	return key
}
