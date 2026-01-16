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

	workers := []Worker{
		// Wakes up relevant federation senders for new events
		NewEventsIterator(log, cfg, db, notifiers),
		// Federation sender per remote homeserver
		NewFederationSender(log, cfg, db, notifiers, fclient),
		// Compacts notification versions for users
		// NewCompactNotificationIterator(log, cfg, db, notifiers),

		// Profile changes from accounts -> member events in rooms
		NewProfileChangeIterator(log, cfg, db, notifiers),

		// Uses push rules from accounts -> push notifications for new events
		// NewPushNotificationIterator(log, cfg, db, notifiers),

		// Device changes from accounts -> internal to-device change notifications
		NewDeviceChangeIterator(log, cfg, db, notifiers),
		// Join events from rooms -> internal to-device change notifications (w/devices from accounts)
		NewDeviceJoinEventIterator(log, cfg, db, notifiers),

		// Presence change -> internal to-device presence notifications
		NewPresenceChangeIterator(log, cfg, db, notifiers),
		// Presence timeouts -> presence changes
		NewPresenceTimeoutIterator(log, cfg, db, notifiers),
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
