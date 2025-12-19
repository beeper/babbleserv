package workers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

type Worker interface {
	Start()
	Stop()
}

var _ Worker = (*iteratorWorker)(nil)

type iteratorWorker struct {
	name string

	log       zerolog.Logger
	config    config.BabbleConfig
	db        *databases.Databases
	notifiers *notifier.Notifiers

	lockName    string
	lockRetry   time.Duration
	lockTimeout time.Duration

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	handler func(lock.Lock)
}

func NewWorker(
	name string,
	logger zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	lockName string,
	lockRetry, lockTimeout time.Duration,
) iteratorWorker {
	log := logger.With().
		Str("worker", name).
		Logger()

	return iteratorWorker{
		name:        name,
		log:         log,
		config:      cfg,
		db:          db,
		notifiers:   notifiers,
		lockName:    lockName,
		lockRetry:   lockRetry,
		lockTimeout: lockTimeout,
	}
}

func (w *iteratorWorker) Start() {
	if w.ctx != nil {
		panic(fmt.Errorf("cannot call %s.Start twice", w.name))
	}
	w.ctx, w.cancel = context.WithCancel(w.log.WithContext(context.Background()))

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		util.PanicRetryLoop(w.ctx, w.log, func() {
			lock.WithLock(w.ctx, w.db.System, w.lockName, lock.LockOptions{
				RetryInterval: w.lockRetry,
				Timeout:       w.lockTimeout,
			}, w.handler)
		})
	}()
}

func (w *iteratorWorker) Stop() {
	w.cancel()
	w.wg.Wait()
	w.log.Info().Msg("Stopped")
}
