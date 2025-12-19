package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
)

type ServersDirectory struct {
	log zerolog.Logger

	// Server name to sync positions, similar to user devices
	//
	// key: (ServerName)
	// value: types.VersionMap (as msgpack []byte)
	syncPositions subspace.Subspace
}

func NewServersDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ServersDirectory {
	serversDir, err := parentDir.CreateOrOpen(db, []string{"servers"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "servers").Logger()
	log.Debug().
		Bytes("prefix", serversDir.Bytes()).
		Msg("Init system/servers directory")

	return &ServersDirectory{
		log:           log,
		syncPositions: serversDir.Sub("itp"),
	}
}

func (s *ServersDirectory) KeyForServerPosition(serverName string) fdb.Key {
	return s.syncPositions.Pack(tuple.Tuple{serverName})
}

func (s *ServersDirectory) PositionKeyToServer(key fdb.Key) string {
	tup, _ := s.syncPositions.Unpack(key)
	return tup[0].(string)
}

func (s *ServersDirectory) RangeForServerPositions() fdb.Range {
	return s.syncPositions
}
