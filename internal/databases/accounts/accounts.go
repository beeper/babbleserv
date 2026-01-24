package accounts

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/accounts/accountdata"
	"github.com/beeper/babbleserv/internal/databases/accounts/devices"
	"github.com/beeper/babbleserv/internal/databases/accounts/pushrules"
	"github.com/beeper/babbleserv/internal/databases/accounts/tokens"
	"github.com/beeper/babbleserv/internal/databases/accounts/users"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION = 710

type AccountsDatabase struct {
	log      zerolog.Logger
	db       fdb.Database
	config   config.BabbleConfig
	notifier *notifier.Notifier

	users       *users.UsersDirectory
	tokens      *tokens.TokensDirectory
	devices     *devices.DevicesDirectory
	accountdata *accountdata.AccountDataDirectory
	pushrules   *pushrules.PushRulesDirectory
}

func NewAccountsDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	notifier *notifier.Notifier,
) *AccountsDatabase {
	log := logger.With().
		Str("database", "accounts").
		Logger()

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.Accounts.Database.ClusterFilePath)
	log.Info().
		Str("cluster_file", cfg.Accounts.Database.ClusterFilePath).
		Msg("Connecting to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Accounts.Database.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Accounts.Database.TransactionRetryLimit)

	accountsDir, err := directory.CreateOrOpen(db, []string{"accounts"}, nil)
	if err != nil {
		panic(err)
	}

	log.Debug().
		Bytes("prefix", accountsDir.Bytes()).
		Msg("Init accounts directory")

	return &AccountsDatabase{
		log:      log,
		db:       db,
		config:   cfg,
		notifier: notifier,

		users:       users.NewUsersDirectory(cfg, log, db, accountsDir),
		tokens:      tokens.NewTokensDirectory(log, db, accountsDir),
		devices:     devices.NewDevicesDirectory(log, db, accountsDir),
		accountdata: accountdata.NewAccountDataDirectory(log, db, accountsDir),
		pushrules:   pushrules.NewPushRulesDirectory(log, db, accountsDir),
	}
}

func (a *AccountsDatabase) Stop() {
}

func (a *AccountsDatabase) getTxnLogContext(ctx context.Context, name string) zerolog.Context {
	return zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("database", "rooms").
		Str("transaction", name)
}

func (a *AccountsDatabase) GetTimeForVersion(ctx context.Context, version tuple.Versionstamp) (*time.Time, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*time.Time, error) {
		time := util.TxnGetTimeForVersion(txn, version)
		return time, nil
	})
}
