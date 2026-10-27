package workers

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/ingest"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	ingestJobScanInterval = time.Minute
	ingestJobScanPage     = 500
)

var _ Worker = (*IngestJobWorker)(nil)

// IngestJobWorker resumes rooms' ingest jobs whose runner stopped, as one in a process that crashed:
// it scans the jobs at startup and every minute, and runs each whose lock is free. Each process
// running workers scans, the job locks keep each job to one runner.
type IngestJobWorker struct {
	log  zerolog.Logger
	db   *databases.Databases
	jobs *ingest.Jobs

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewIngestJobWorker(logger zerolog.Logger, db *databases.Databases, jobs *ingest.Jobs) *IngestJobWorker {
	log := logger.With().Str("worker", "IngestJobWorker").Logger()
	ctx, cancel := context.WithCancel(log.WithContext(context.Background()))
	return &IngestJobWorker{log: log, db: db, jobs: jobs, ctx: ctx, cancel: cancel}
}

func (w *IngestJobWorker) Start() {
	w.wg.Go(func() {
		ticker := time.NewTicker(ingestJobScanInterval)
		defer ticker.Stop()
		for {
			w.scan()
			select {
			case <-ticker.C:
			case <-w.ctx.Done():
				return
			}
		}
	})
}

func (w *IngestJobWorker) Stop() {
	w.cancel()
	w.wg.Wait()
	w.log.Info().Msg("Stopped")
}

func (w *IngestJobWorker) scan() {
	defer util.RecoverPanic(&w.log, nil)
	var after id.RoomID
	resumed := 0
	for {
		roomIDs, err := w.db.Rooms.IngestJobRoomIDs(w.ctx, after, ingestJobScanPage)
		if err != nil {
			if w.ctx.Err() == nil {
				w.log.Err(err).Msg("Failed to scan ingest jobs")
			}
			return
		}
		for _, roomID := range roomIDs {
			w.jobs.Run(roomID)
		}
		resumed += len(roomIDs)
		if len(roomIDs) < ingestJobScanPage {
			break
		}
		after = roomIDs[len(roomIDs)-1]
	}
	if resumed > 0 {
		w.log.Debug().Int("jobs", resumed).Msg("Ran ingest jobs found by the scan")
	}
}
