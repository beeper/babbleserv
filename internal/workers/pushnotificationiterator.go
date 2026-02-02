package workers

import (
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules/pushgateway"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	pushNotificationIteratorPositionsKey = "PushNotificationIteratorPositions"
	pushNotificationIteratorLockName     = "PushNotificationIteratorLock"
	pushNotificationIteratorLockRetry    = time.Second * 5
	pushNotificationIteratorLockTimeout  = time.Second * 10
	pushNotificationIteratorBatchSize    = 10
)

// The PushNotificationIterator sends push notifications to local user devices for new events.
// It also cleans up old notification entries to prevent indefinite growth.
type PushNotificationIterator struct {
	iteratorWorker
}

func NewPushNotificationIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *PushNotificationIterator {
	n := &PushNotificationIterator{NewWorker(
		"PushNotificationIterator", log, cfg, db, notifiers,
		pushNotificationIteratorLockName,
		pushNotificationIteratorLockRetry,
		pushNotificationIteratorLockTimeout,
	)}
	n.handler = n.handleNotificationsLoop
	return n
}

func (n *PushNotificationIterator) handleNotificationsLoop(lock lock.Lock) {
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
		case <-time.After(pushNotificationIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (n *PushNotificationIterator) handleNotifications(lock lock.Lock) {
	startVersion, err := n.db.System.GetIteratorPositions(n.ctx, pushNotificationIteratorPositionsKey)
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
			Limit: pushNotificationIteratorBatchSize,
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
			Msg("Handling push notification batch")

		if err := n.sendPushNotificationsForEvents(newEventTups); err != nil {
			n.log.Err(err).Msg("Failed to send push notifications")
		}

		currentVersion = newEventTups[len(newEventTups)-1].Version

		if len(newEventTups) < pushNotificationIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = n.db.System.UpdateIteratorPositions(n.ctx, pushNotificationIteratorPositionsKey, currentVersion, lock.TxnRefresh)
	if err != nil {
		n.log.Err(err).Msg("Failed to update current position")
		return
	}
}

func (n *PushNotificationIterator) sendPushNotificationsForEvents(tups []types.EventTupWithVersion) error {
	// Group events by room
	eventsByRoom := make(map[id.RoomID][]types.EventTupWithVersion)
	for _, tup := range tups {
		eventsByRoom[tup.RoomID] = append(eventsByRoom[tup.RoomID], tup)
	}

	var wg sync.WaitGroup

	for roomID, eventTups := range eventsByRoom {
		// Get local joined users in room
		memberships, err := n.db.Rooms.GetCurrentRoomLocalJoinedMemberships(n.ctx, roomID)
		if err != nil {
			return err
		}

		for userID := range memberships {
			for _, eventTup := range eventTups {
				// Check if notification exists at this event's version
				notif, err := n.db.Rooms.GetNotificationAtVersion(n.ctx, userID, roomID, eventTup.Version)
				if err != nil {
					return err
				}
				if notif != nil {
					wg.Add(1)
					go func(userID id.UserID, eventTup types.EventTupWithVersion, notif types.Notifications) {
						defer wg.Done()
						n.sendPushForUser(userID, eventTup, notif)
					}(userID, eventTup, *notif)
				}
			}
		}
	}

	// Wait for all push notifications to be sent before returning
	wg.Wait()

	return nil
}

func (n *PushNotificationIterator) sendPushForUser(userID id.UserID, eventTup types.EventTupWithVersion, notif types.Notifications) {
	log := n.log.With().
		Stringer("user_id", userID).
		Stringer("event_id", eventTup.EventID).
		Stringer("room_id", eventTup.RoomID).
		Logger()

	// Get user's pushers from accounts db
	pushers, err := n.db.Accounts.GetPushersForUser(n.ctx, userID)
	if err != nil {
		log.Err(err).Msg("Failed to get pushers for user")
		return
	}
	if len(pushers) == 0 {
		return
	}

	// Get full event for push content
	ev, err := n.db.Rooms.GetEvent(n.ctx, eventTup.EventID)
	if err != nil {
		log.Err(err).Msg("Failed to get event")
		return
	}
	if ev == nil {
		log.Warn().Msg("Event not found")
		return
	}

	// Get current notification counts for this user/room (up to this event)
	notifCount, highlightCount, err := n.db.Rooms.SumNotifications(n.ctx, userID, eventTup.RoomID, eventTup.Version)
	if err != nil {
		log.Err(err).Msg("Failed to sum notifications")
		return
	}

	// Determine priority based on highlight
	priority := pushgateway.PushPriorityLow
	if notif.Highlight > 0 {
		priority = pushgateway.PushPriorityHigh
	}

	// Send to each pusher
	for _, pusher := range pushers {
		if pusher.Kind == nil || *pusher.Kind != pushgateway.PusherKindHTTP {
			continue
		}

		url := pusher.Data.URL()
		if url == "" {
			log.Warn().Msg("HTTP pusher has no URL")
			continue
		}

		// Build PushNotification
		pushNotif := &pushgateway.PushNotification{
			EventID:  ev.ID,
			RoomID:   ev.RoomID,
			Sender:   ev.Sender,
			Type:     ev.Type.String(),
			Priority: priority,
			Counts: &pushgateway.NotificationCounts{
				Unread: notifCount + highlightCount,
			},
			Devices: []pushgateway.Device{{
				BaseDevice: pushgateway.BaseDevice{
					AppID:   pusher.AppID,
					PushKey: pusher.PushKey,
					Data:    pusher.Data.ConvertToNotificationData(),
				},
			}},
		}

		// Include content unless event_id_only format
		if pusher.Data.Format() != pushgateway.PushFormatEventIDOnly {
			pushNotif.Content = ev.Content
			pushNotif.SenderDisplayName = n.getSenderDisplayName(ev)
			pushNotif.RoomName = n.getRoomName(eventTup.RoomID)
		}

		if err := pushNotif.Push(n.ctx, url); err != nil {
			// If rejected, delete the pusher
			if errors.Is(err, pushgateway.ErrPushRejected) {
				log.Info().Str("pushkey", pusher.PushKey).Msg("Push rejected, deleting pusher")
				if err := n.db.Accounts.DeletePusherForUser(n.ctx, userID, pusher.PushKey); err != nil {
					log.Err(err).Str("pushkey", pusher.PushKey).Msg("Failed to delete rejected pusher")
				}
			} else {
				log.Err(err).Str("pushkey", pusher.PushKey).Msg("Failed to send push notification")
			}
		} else {
			log.Debug().Str("pushkey", pusher.PushKey).Msg("Sent push notification")
		}
	}

	log.Debug().Msg("Sent user push notifications")
}

func (n *PushNotificationIterator) getSenderDisplayName(ev *types.Event) string {
	// Try to get display name from profile
	profile, err := n.db.Accounts.GetUserProfile(n.ctx, ev.Sender)
	if err == nil && profile != nil && profile.DisplayName != "" {
		return profile.DisplayName
	}
	return ev.Sender.Localpart()
}

func (n *PushNotificationIterator) getRoomName(roomID id.RoomID) string {
	// Try to get room name from state
	roomNameEvent, err := n.db.Rooms.GetCurrentRoomStateEvent(n.ctx, roomID, types.StateTup{
		Type:     event.StateRoomName,
		StateKey: "",
	})
	if err == nil && roomNameEvent != nil {
		return gjson.GetBytes(roomNameEvent.Content, "name").String()
	}
	return ""
}
