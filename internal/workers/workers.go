package workers

import (
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
)

type Workers struct {
	log    zerolog.Logger
	config config.BabbleConfig

	db        *databases.Databases
	notifiers *notifier.Notifiers

	workers []Worker
}

func NewWorkers(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	fclient fclient.FederationClient,
) *Workers {
	log := logger.With().
		Str("component", "workers").
		Logger()

	workers := []Worker{}

	if cfg.Rooms.Enabled {
		workers = append(workers,
			NewEventsIterator(log, cfg, db, notifiers),
			NewFederationSender(log, cfg, db, notifiers, fclient),
		)
	}

	if cfg.Accounts.Enabled {
		// ProfileChangeIterator accounts profile changes -> room member events
		if cfg.Rooms.Enabled {
			workers = append(workers, NewProfileChangeIterator(log, cfg, db, notifiers))
		}
		// DeviceChangeIterator accounts device changes -> transient to device
		if cfg.Transient.Enabled {
			workers = append(workers, NewDeviceChangeIterator(log, cfg, db, notifiers))
		}
	}

	if cfg.Transient.Enabled {
		workers = append(workers,
			NewPresenceChangeIterator(log, cfg, db, notifiers),
			NewPresenceTimeoutIterator(log, cfg, db, notifiers),
		)
	}

	return &Workers{
		log:       log,
		config:    cfg,
		db:        db,
		notifiers: notifiers,
		workers:   workers,
	}
}

func (w *Workers) Start() {
	w.log.Info().Msg("Starting workers...")
	for _, w := range w.workers {
		w.Start()
	}
}

func (w *Workers) Stop() {
	w.log.Info().Msg("Stopping workers...")
	for _, w := range w.workers {
		w.Stop()
	}
}
