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

	db       *databases.Databases
	notifier *notifier.Notifier

	workers []Worker
}

func NewWorkers(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	notif *notifier.Notifier,
	fclient fclient.FederationClient,
) *Workers {
	log := logger.With().
		Str("component", "workers").
		Logger()

	workers := []Worker{
		NewEventsIterator(log, cfg, db, notif),
		NewFederationSender(log, cfg, db, notif, fclient),
	}

	return &Workers{
		log:      log,
		config:   cfg,
		db:       db,
		notifier: notif,
		workers:  workers,
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
