package transient

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/transient/todevice"
	"github.com/beeper/babbleserv/internal/notifier"
)

const API_VERSION = 710

type TransientDatabase struct {
	log      zerolog.Logger
	db       fdb.Database
	config   config.BabbleConfig
	notifier *notifier.Notifier

	todevice *todevice.ToDeviceDirectory
}

func NewTransientDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	notifier *notifier.Notifier,
) *TransientDatabase {
	log := logger.With().
		Str("database", "transient").
		Logger()

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.Transient.Database.ClusterFilePath)
	log.Info().
		Str("cluster_file", cfg.Transient.Database.ClusterFilePath).
		Msg("Connecting to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Transient.Database.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Transient.Database.TransactionRetryLimit)

	transientDir, err := directory.CreateOrOpen(db, []string{"transient"}, nil)
	if err != nil {
		panic(err)
	}

	log.Debug().
		Bytes("prefix", transientDir.Bytes()).
		Msg("Init transient directory")

	return &TransientDatabase{
		log:      log,
		db:       db,
		config:   cfg,
		notifier: notifier,

		todevice: todevice.NewToDeviceDirectory(log, db, transientDir),
	}
}

func (t *TransientDatabase) Stop() {
}
