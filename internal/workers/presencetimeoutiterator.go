package workers

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	presenceTimeoutIteratorPositionsKey = "PresenceTimeoutIteratorPositions"
	presenceTimeoutIteratorLockName     = "PresenceTimeoutIteratorLock"
	presenceTimeoutIteratorLockRetry    = time.Second * 5
	presenceTimeoutIteratorLockTimeout  = time.Second * 10
	presenceTimeoutIteratorBatchSize    = 10
)

// PresenceTimeoutIterator processes due timeout rows in bounded batches,
// atomically expiring idle users or rescheduling users who are still active.
type PresenceTimeoutIterator struct {
	iteratorWorker
}

func NewPresenceTimeoutIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *PresenceTimeoutIterator {
	p := &PresenceTimeoutIterator{NewWorker(
		"PresenceTimeoutIterator", log, cfg, db, notifiers,
		presenceTimeoutIteratorLockName,
		presenceTimeoutIteratorLockRetry,
		presenceTimeoutIteratorLockTimeout,
	)}
	p.handler = p.handlePresenceTimeoutsLoop
	return p
}

func (p *PresenceTimeoutIterator) handlePresenceTimeoutsLoop(lock lock.Lock) {
	// Cold start: process any pending timeouts
	p.handlePresenceTimeouts(lock)

	loopTick := time.NewTicker(p.config.Transient.PresenceTimeoutCheckInterval)
	defer loopTick.Stop()

	lockTick := time.NewTicker(presenceTimeoutIteratorLockRetry)
	defer lockTick.Stop()

	for {
		select {
		case <-p.ctx.Done():
			lock.Release()
			return
		case <-loopTick.C:
			p.handlePresenceTimeouts(lock)
		case <-lockTick.C:
			// Periodic check and lock refresh
			lock.Refresh()
			p.handlePresenceTimeouts(lock)
		}
	}
}

func (p *PresenceTimeoutIterator) handlePresenceTimeouts(lock lock.Lock) {
	// Refresh lock before processing
	lock.Refresh()

	now := time.Now().UTC()

	timeouts, err := p.db.Transient.GetPresenceTimeouts(p.ctx, now, presenceTimeoutIteratorBatchSize)
	if err != nil {
		p.log.Err(err).Msg("Failed to get presence timeouts")
		return
	}

	if len(timeouts) == 0 {
		p.log.Trace().Msg("No presence timeouts to process")
		return
	}

	p.log.Info().Int("timeouts", len(timeouts)).Msg("Processing presence timeouts")
	for _, timeout := range timeouts {
		lock.Refresh()
		if err := p.db.Transient.HandlePresenceTimeout(p.ctx, timeout, now); err != nil {
			p.log.Err(err).Stringer("user_id", timeout.UserID).
				Msg("Failed to handle presence timeout")
		}
	}
}
