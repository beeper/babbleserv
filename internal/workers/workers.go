package workers

import (
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
)

type Worker interface {
	Start()
	Stop()
}

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
