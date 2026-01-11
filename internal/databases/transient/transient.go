package transient

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/transient/presence"
	"github.com/beeper/babbleserv/internal/databases/transient/todevice"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION = 710

type TransientDatabase struct {
	log      zerolog.Logger
	db       fdb.Database
	config   config.BabbleConfig
	notifier *notifier.Notifier

	todevice *todevice.ToDeviceDirectory
	presence *presence.PresenceDirectory
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
		presence: presence.NewPresenceDirectory(log, db, transientDir),
	}
}

func (t *TransientDatabase) Stop() {
}

func (t *TransientDatabase) GetTimeForVersion(ctx context.Context, version tuple.Versionstamp) (*time.Time, error) {
	return util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) (*time.Time, error) {
		time := util.TxnGetTimeForVersion(txn, version)
		return time, nil
	})
}
