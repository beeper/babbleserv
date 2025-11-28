package system

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/system/servers"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const API_VERSION = 710

var _ lock.LockingDatabase = (*SystemDatabase)(nil)

type SystemDatabase struct {
	log zerolog.Logger
	db  fdb.Database

	servers *servers.ServersDirectory

	// Prefix for distributed locks (util/lock)
	locks subspace.Subspace

	// Prefix for worker iterator positions
	iteratorPositions subspace.Subspace
}

func NewSystemDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
) *SystemDatabase {
	log := logger.With().
		Str("database", "system").
		Logger()

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.System.Database.ClusterFilePath)
	log.Info().
		Str("cluster_file", cfg.System.Database.ClusterFilePath).
		Msg("Connecting to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.System.Database.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.System.Database.TransactionRetryLimit)

	systemDir, err := directory.CreateOrOpen(db, []string{"system"}, nil)
	if err != nil {
		panic(err)
	}

	log.Debug().
		Bytes("prefix", systemDir.Bytes()).
		Msg("Init system directory")

	return &SystemDatabase{
		log: log,
		db:  db,

		servers: servers.NewServersDirectory(log, db, systemDir),

		locks:             systemDir.Sub("lck"),
		iteratorPositions: systemDir.Sub("itp"),
	}
}

func (s *SystemDatabase) GetLockPrimitives() (fdb.Database, subspace.Subspace) {
	return s.db, s.locks
}
