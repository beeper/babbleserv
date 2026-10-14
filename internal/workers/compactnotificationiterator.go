package workers

import (
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

	roomIDToCount map[id.RoomID]int
	roomIDToTimer map[id.RoomID]*time.Timer
	timedOutRooms chan id.RoomID
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
		roomIDToCount: make(map[id.RoomID]int, 100),
		roomIDToTimer: make(map[id.RoomID]*time.Timer, 100),
		timedOutRooms: make(chan id.RoomID),
	}
	n.handler = n.handleNotificationsLoop
	return n
}

func (n *CompactNotificationIterator) handleNotificationsLoop(lock lock.Lock) {
	newEventsCh := n.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer n.notifiers.Unsubscribe(newEventsCh)

	refreshTicker := time.NewTicker(compactNotificationIteratorLockRetry)
	defer refreshTicker.Stop()

	for {
		select {
		case <-n.ctx.Done():
			lock.Release()
			return
		case change := <-newEventsCh:
			n.unlockedHandleChange(lock, change)
		case roomID := <-n.timedOutRooms:
			n.log.Debug().
				Stringer("room_id", roomID).
				Dur("timeout", n.config.Rooms.CompactRoomNotificationsTimeout).
				Msg("Compacting room notifications after timeout")
			n.compactNotificationsForRoom(roomID)
		case <-refreshTicker.C:
			lock.Refresh()
		}
	}
}

func (n *CompactNotificationIterator) unlockedHandleChange(lock lock.Lock, change notifier.Change) {
	for _, roomID := range change.RoomIDs {
		// Bump counter, if > the notification limit, compact room now
		n.roomIDToCount[roomID]++
		if n.roomIDToCount[roomID] > n.config.Rooms.MaxNotificationsPerUserRoom {
			n.log.Debug().
				Stringer("room_id", roomID).
				Int("max_notifications", n.config.Rooms.MaxNotificationsPerUserRoom).
				Msg("Compacting room notifications after sufficient traffic")
			n.compactNotificationsForRoom(roomID)
			continue
		}

		// Less than configured changes in this room, (re)start the quiet period
		// timeout which hands the room back to the loop for compaction
		if timer, ok := n.roomIDToTimer[roomID]; ok {
			timer.Reset(n.config.Rooms.CompactRoomNotificationsTimeout)
			continue
		}
		n.roomIDToTimer[roomID] = time.AfterFunc(n.config.Rooms.CompactRoomNotificationsTimeout, func() {
			select {
			case n.timedOutRooms <- roomID:
			case <-n.ctx.Done():
			}
		})
	}
}

func (n *CompactNotificationIterator) compactNotificationsForRoom(roomID id.RoomID) {
	if timer, ok := n.roomIDToTimer[roomID]; ok {
		timer.Stop()
		delete(n.roomIDToTimer, roomID)
	}
	delete(n.roomIDToCount, roomID)

	go func() {
		if err := n.compactNotificationsForRoomMembers(roomID); err != nil {
			n.log.Err(err).Stringer("room_id", roomID).Msg("Failed to compact room notifications")
		}
	}()
}

func (n *CompactNotificationIterator) compactNotificationsForRoomMembers(roomID id.RoomID) error {
	memberships, err := n.db.Rooms.LocalJoinedMembers(n.ctx, roomID)
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
