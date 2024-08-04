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
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/notifier"
)

type Databases struct {
	log   zerolog.Logger
	Rooms *rooms.RoomsDatabase
}

func NewDatabases(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	notifier *notifier.Notifier,
) *Databases {
	log := logger.With().
		Str("component", "databases").
		Logger()

	return &Databases{
		log:   log,
		Rooms: rooms.NewRoomsDatabase(cfg, log, notifier),
	}
}

func (d *Databases) Start() {
	// noop
}

func (d *Databases) Stop() {
	d.log.Info().Msg("Stopping databases...")
	d.Rooms.Stop()
}
