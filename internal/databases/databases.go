// Collector struct for all our databases. Each database is logically separate
// and does not have to be within the same FoundationDB cluster. Any cross DB
// functionality lives here (ie sync). Currently we have:
//
// rooms - events, receipts, room account data
// TBC users - user profiles, global cacount data
// TBC devices - to-device events
// TBC presence - presence status

package databases

import (
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/accounts"
	"github.com/beeper/babbleserv/internal/databases/media"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/notifier"
)

type Databases struct {
	log zerolog.Logger

	Rooms     *rooms.RoomsDatabase
	Accounts  *accounts.AccountsDatabase
	Transient *transient.TransientDatabase
	Media     *media.MediaDatabase
}

func NewDatabases(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	notifiers *notifier.Notifiers,
) *Databases {
	log := logger.With().
		Str("component", "databases").
		Logger()

	dbs := Databases{log: log}

	if cfg.Rooms.Enabled {
		dbs.Rooms = rooms.NewRoomsDatabase(cfg, log, notifiers)
	}
	if cfg.Accounts.Enabled {
		dbs.Accounts = accounts.NewAccountsDatabase(cfg, log)
	}
	if cfg.Transient.Enabled {
		dbs.Transient = transient.NewTransientDatabase(cfg, log)
	}
	if cfg.Media.Enabled {
		dbs.Media = media.NewMediaDatabase(cfg, log)
	}

	return &dbs
}

func (d *Databases) Start() {
	// noop
}

func (d *Databases) Stop() {
	d.log.Info().Msg("Stopping databases...")

	if d.Rooms != nil {
		d.Rooms.Stop()
	}
	if d.Accounts != nil {
		d.Accounts.Stop()
	}
	if d.Transient != nil {
		d.Transient.Stop()
	}
	if d.Media != nil {
		d.Media.Stop()
	}
}
