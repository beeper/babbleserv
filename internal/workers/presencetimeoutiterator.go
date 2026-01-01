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

// The PresenceTimeoutIterator handles timing out presence status (online -> unavailable), we do
// this by storing (timeoutms, userid) whenever we set presence status to online. We periodically
// fetch all values where timeoutms < now and for each:
// - if lastActive > timeout, clear and key and set a new (now+timeoutms, userid) key
// - if lastActive < timeout, clear and update presence state, if online, to unavailable
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

	// Get all users with timeouts up to now
	userIDs, err := p.db.Transient.GetPresenceTimeouts(p.ctx, now)
	if err != nil {
		p.log.Err(err).Msg("Failed to get presence timeouts")
		return
	}

	if len(userIDs) == 0 {
		p.log.Trace().Msg("No presence timeouts to process")
		return
	}

	p.log.Info().
		Int("users", len(userIDs)).
		Msg("Processing presence timeouts")

	for _, userID := range userIDs {
		// Refresh lock periodically
		lock.Refresh()

		presence, err := p.db.Transient.GetUserPresence(p.ctx, userID)
		if err != nil {
			p.log.Err(err).
				Str("user_id", userID.String()).
				Msg("Failed to get user presence")
			continue
		}

		// If no presence, skip (shouldn't happen but be safe)
		if presence == nil {
			p.log.Warn().
				Str("user_id", userID.String()).
				Msg("User has timeout but no presence")
			continue
		}

		if presence.LastActive.After(now.Add(-p.config.Transient.PresenceTimeout)) {
			// TODO: set these in a batch
			timeout := now.Add(p.config.Transient.PresenceTimeout)
			if err := p.db.Transient.SetPresenceTimeout(p.ctx, timeout, userID); err != nil {
				p.log.Err(err).
					Str("user_id", userID.String()).
					Msg("Failed to reschedule presence timeout")

			} else {
				p.log.Debug().
					Stringer("user_id", userID).
					Time("last_active", presence.LastActive).
					Msg("User still active, rescheduled timeout")
			}
		} else if presence.Presence == "online" {
			p.log.Info().
				Stringer("user_id", userID).
				Time("last_active", presence.LastActive).
				Msg("User presence timed out, setting to unavailable")

			if err := p.db.Transient.UpdateUserPresenceState(p.ctx, userID, "unavailable"); err != nil {
				p.log.Err(err).
					Stringer("user_id", userID).
					Msg("Failed to update presence to unavailable")
			}
		}
	}

	// Clear all processed timeouts
	if err := p.db.Transient.ClearPresenceTimeouts(p.ctx, now); err != nil {
		p.log.Err(err).Msg("Failed to clear presence timeouts")
	}
}
