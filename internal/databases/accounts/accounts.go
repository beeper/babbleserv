package accounts

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/accounts/accountdata"
	"github.com/beeper/babbleserv/internal/databases/accounts/devices"
	"github.com/beeper/babbleserv/internal/databases/accounts/tokens"
	"github.com/beeper/babbleserv/internal/databases/accounts/users"
	"github.com/beeper/babbleserv/internal/notifier"
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
}

func NewAccountsDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
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
		log:    log,
		db:     db,
		config: cfg,

		users:       users.NewUsersDirectory(log, db, accountsDir),
		tokens:      tokens.NewTokensDirectory(log, db, accountsDir),
		devices:     devices.NewDevicesDirectory(log, db, accountsDir),
		accountdata: accountdata.NewAccountDataDirectory(log, db, accountsDir),
	}
}

func (a *AccountsDatabase) Stop() {
}
