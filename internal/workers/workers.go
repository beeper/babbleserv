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
			// Wakes up relevant federation senders for new events
			NewEventsIterator(log, cfg, db, notifiers),
			// Federation sender per remote homeserver
			NewFederationSender(log, cfg, db, notifiers, fclient),
		)
	}

	if cfg.Accounts.Enabled && cfg.Rooms.Enabled {
		// Profile changes from accounts -> member events in rooms
		workers = append(workers, NewProfileChangeIterator(log, cfg, db, notifiers))
	}

	if cfg.Accounts.Enabled && cfg.Transient.Enabled {
		// Device changes from accounts -> internal to-device change notifications
		workers = append(workers, NewDeviceChangeIterator(log, cfg, db, notifiers))
	}

	if cfg.Accounts.Enabled && cfg.Rooms.Enabled && cfg.Transient.Enabled {
		// Join events from rooms -> internal to-device change notifications (w/devices from accounts)
		workers = append(workers, NewDeviceJoinEventIterator(log, cfg, db, notifiers))
	}

	if cfg.Transient.Enabled {
		workers = append(workers,
			// Presence change -> internal to-device presence notifications
			NewPresenceChangeIterator(log, cfg, db, notifiers),
			// Presence timeouts -> presence changes
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
