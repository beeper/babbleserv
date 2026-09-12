package workers

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	compactNotificationIteratorLockName    = "CompactNotificationIteratorLock"
	compactNotificationIteratorLockRetry   = time.Second * 5
	compactNotificationIteratorLockTimeout = time.Second * 10
)

// The CompactNotificationIterator is a singleton background worker that iterates over events
// and compacts notification versions for affected users. This reduces the number of
// notification entries that need to be summed during sync.
type CompactNotificationIterator struct {
	iteratorWorker

	roomIDToCount  map[id.RoomID]int
	roomIDToCancel map[id.RoomID]context.CancelFunc
}

func NewCompactNotificationIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *CompactNotificationIterator {
	n := &CompactNotificationIterator{
		iteratorWorker: NewWorker(
			"CompactNotificationIterator", log, cfg, db, notifiers,
			compactNotificationIteratorLockName,
			compactNotificationIteratorLockRetry,
			compactNotificationIteratorLockTimeout,
		),
		roomIDToCount:  make(map[id.RoomID]int, 100),
		roomIDToCancel: make(map[id.RoomID]context.CancelFunc, 100),
	}
	n.handler = n.handleNotificationsLoop
	return n
}

func (n *CompactNotificationIterator) handleNotificationsLoop(lock lock.Lock) {
	newEventsCh := n.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer n.notifiers.Unsubscribe(newEventsCh)

	for {
		select {
		case <-n.ctx.Done():
			lock.Release()
			return
		case change := <-newEventsCh:
			n.unlockedHandleChange(lock, change)
		case <-time.After(compactNotificationIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (n *CompactNotificationIterator) unlockedHandleChange(lock lock.Lock, change notifier.Change) {
	for _, roomID := range change.RoomIDs {
		// First cancel any in-flight timeout for the room
		if cancel, ok := n.roomIDToCancel[roomID]; ok {
			cancel()
		}

		// Bump counter, if > the notification limit, compact room and exit
		n.roomIDToCount[roomID]++
		if n.roomIDToCount[roomID] > n.config.Rooms.MaxNotificationsPerUserRoom {
			n.log.Debug().
				Stringer("room_id", roomID).
				Int("max_notifications", n.config.Rooms.MaxNotificationsPerUserRoom).
				Msg("Compacting room notifications after sufficient traffic")
			n.compactNotificationsForRoom(roomID)
			return
		}

		// Less than configured changes in this room, set background timeout to
		ctx, cancel := context.WithCancel(context.Background())
		n.roomIDToCancel[roomID] = cancel
		go func() {
			select {
			case <-ctx.Done():
				n.log.Trace().Msg("Room compact timeout canceled")
				return
			case <-time.After(n.config.Rooms.CompactRoomNotificationsTimeout):
				n.log.Debug().
					Stringer("room_id", roomID).
					Dur("timeout", n.config.Rooms.CompactRoomNotificationsTimeout).
					Msg("Compacting room notifications after timeout")
				n.compactNotificationsForRoom(roomID)
			}
		}()
	}
}

func (n *CompactNotificationIterator) compactNotificationsForRoom(roomID id.RoomID) error {
	memberships, err := n.db.Rooms.GetCurrentRoomLocalJoinedMemberships(n.ctx, roomID)
	if err != nil {
		return err
	}

	for userID := range memberships {
		if err := n.db.Rooms.CompactNotifications(n.ctx, userID, roomID); err != nil {
			return err
		}
	}

	return nil
}
