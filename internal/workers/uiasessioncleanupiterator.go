package workers

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const uiaCleanupBatchSize = 100

type UIASessionCleanupIterator struct{ iteratorWorker }

func NewUIASessionCleanupIterator(log zerolog.Logger, cfg config.BabbleConfig, db *databases.Databases, notifiers *notifier.Notifiers) *UIASessionCleanupIterator {
	w := &UIASessionCleanupIterator{NewWorker("UIASessionCleanupIterator", log, cfg, db, notifiers,
		"UIASessionCleanupIteratorLock", 5*time.Second, 30*time.Second)}
	w.handler = w.handleCleanupLoop
	return w
}

func (w *UIASessionCleanupIterator) handleCleanupLoop(workerLock lock.Lock) {
	defer workerLock.Release()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		workerLock.Refresh()
		sessions, sessionsErr := w.db.Accounts.CleanupExpiredUIASessions(w.ctx, uiaCleanupBatchSize)
		if sessionsErr != nil {
			w.log.Err(sessionsErr).Msg("Failed to clean expired UIA sessions")
		}
		tokens, tokensErr := w.db.Accounts.CleanupExpiredAccessTokens(w.ctx, uiaCleanupBatchSize)
		if tokensErr != nil {
			w.log.Err(tokensErr).Msg("Failed to clean expired access tokens")
		}
		if w.ctx.Err() != nil {
			return
		}
		if sessionsErr == nil && tokensErr == nil &&
			(sessions == uiaCleanupBatchSize || tokens == uiaCleanupBatchSize) {
			continue
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
