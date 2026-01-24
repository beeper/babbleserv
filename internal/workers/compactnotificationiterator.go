package workers

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	compactNotificationIteratorPositionsKey = "CompactNotificationIteratorPositions"
	compactNotificationIteratorLockName     = "CompactNotificationIteratorLock"
	compactNotificationIteratorLockRetry    = time.Second * 5
	compactNotificationIteratorLockTimeout  = time.Second * 10
	compactNotificationIteratorBatchSize    = 10
)

// The CompactNotificationIterator is a singleton background worker that iterates over events
// and compacts notification versions for affected users. This reduces the number of
// notification entries that need to be summed during sync.
type CompactNotificationIterator struct {
	iteratorWorker
}

func NewCompactNotificationIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *CompactNotificationIterator {
	n := &CompactNotificationIterator{NewWorker(
		"CompactNotificationIterator", log, cfg, db, notifiers,
		compactNotificationIteratorLockName,
		compactNotificationIteratorLockRetry,
		compactNotificationIteratorLockTimeout,
	)}
	n.handler = n.handleNotificationsLoop
	return n
}

func (n *CompactNotificationIterator) handleNotificationsLoop(lock lock.Lock) {
	newEventsCh := n.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer n.notifiers.Unsubscribe(newEventsCh)

	// Cold start case: handle anything waiting right away
	n.handleNotifications(lock)

	for {
		select {
		case <-n.ctx.Done():
			lock.Release()
			return
		case <-newEventsCh:
			n.handleNotifications(lock)
		case <-time.After(compactNotificationIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (n *CompactNotificationIterator) handleNotifications(lock lock.Lock) {
	startVersion, err := n.db.System.GetIteratorPositions(n.ctx, compactNotificationIteratorPositionsKey)
	if err != nil {
		n.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh the lock before we process each batch
		lock.Refresh()

		newEventTups, err := n.db.Rooms.PaginateAllEventTups(n.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: compactNotificationIteratorBatchSize,
		})
		if err != nil {
			n.log.Err(err).Msg("Failed to paginate events")
			return
		} else if len(newEventTups) == 0 {
			n.log.Trace().Any("fromVersion", currentVersion).Msg("No events found")
			break
		}

		n.log.Debug().
			Int("events", len(newEventTups)).
			Any("fromVersion", currentVersion).
			Msg("Handling notification compaction batch")

		if err := n.compactNotificationsForEvents(newEventTups); err != nil {
			n.log.Err(err).Msg("Failed to compact notifications")
		}

		currentVersion = newEventTups[len(newEventTups)-1].Version

		if len(newEventTups) < compactNotificationIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = n.db.System.UpdateIteratorPositions(n.ctx, compactNotificationIteratorPositionsKey, currentVersion, lock.TxnRefresh)
	if err != nil {
		n.log.Err(err).Msg("Failed to update current position")
		return
	}
}

func (n *CompactNotificationIterator) compactNotificationsForEvents(tups []types.EventTupWithVersion) error {
	// Get unique room IDs from the events
	roomIDs := make(map[id.RoomID]tuple.Versionstamp)
	for _, tup := range tups {
		roomIDs[tup.RoomID] = tup.Version
	}

	// For each room, get local joined users and compact their notifications
	for roomID, upToVersion := range roomIDs {
		memberships, err := n.db.Rooms.GetCurrentRoomLocalJoinedMemberships(n.ctx, roomID)
		if err != nil {
			return err
		}

		for userID := range memberships {
			if err := n.db.Rooms.CompactNotifications(n.ctx, userID, roomID, upToVersion); err != nil {
				return err
			}
		}
	}

	return nil
}
