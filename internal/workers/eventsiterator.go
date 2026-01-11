package workers

import (
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	eventsIteratorPositionsKey = "EventsIteratorPositions"
	eventsIteratorLockName     = "EventsIteratorLock"
	eventsIteratorLockRetry    = time.Second * 5
	eventsIteratorLockTimeout  = time.Second * 10
	eventsIteratorBatchSize    = 10
)

// The EventsIterator is a singleton background worker that iterates over all events ever stored
// by Babbleserv and triggers other things:
// - starts federation senders for servers in rooms with new events
// - sends device change notifications for join/leave events in encrypted rooms
type EventsIterator struct {
	iteratorWorker
}

func NewEventsIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *EventsIterator {
	e := &EventsIterator{NewWorker(
		"EventsIterator", log, cfg, db, notifiers,
		eventsIteratorLockName,
		eventsIteratorLockRetry,
		eventsIteratorLockTimeout,
	)}
	e.handler = e.handleNewEventsLoop
	return e
}

func (e *EventsIterator) handleNewEventsLoop(lock lock.Lock) {
	newEventsCh := e.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer e.notifiers.Unsubscribe(newEventsCh)

	// Cold start case: handle anything waiting right away
	e.handleNewEvents(lock)

	for {
		select {
		case <-e.ctx.Done():
			lock.Release()
			return
		case <-newEventsCh:
			e.handleNewEvents(lock)
		case <-time.After(eventsIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (e *EventsIterator) handleNewEvents(lock lock.Lock) {
	startVersion, err := e.db.System.GetIteratorPositions(e.ctx, eventsIteratorPositionsKey)
	if err != nil {
		e.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh the lock before we process each batch
		lock.Refresh()

		newEventTups, err := e.db.Rooms.PaginateAllEventTups(e.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: eventsIteratorBatchSize,
		})
		if err != nil {
			e.log.Err(err).Msg("Failed to paginate new events")
			return
		} else if len(newEventTups) == 0 {
			e.log.Trace().Any("fromVersion", currentVersion).Msg("No events found")
			break
		}

		e.log.Info().
			Int("events", len(newEventTups)).
			Any("fromVersion", currentVersion).
			Msg("Handling new events batch")

		if err := e.notifyFederationSenders(newEventTups); err != nil {
			e.log.Err(err).Msg("Failed to notify federation senders")
		}

		currentVersion = newEventTups[len(newEventTups)-1].Version

		if len(newEventTups) < eventsIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = e.db.System.UpdateIteratorPositions(e.ctx, eventsIteratorPositionsKey, currentVersion, lock.TxnRefresh)
	if err != nil {
		e.log.Err(err).Msg("Failed to update current position")
		return
	}
}

func (e *EventsIterator) notifyFederationSenders(tups []types.EventTupWithVersion) error {
	// Get unique room IDs
	roomIDs := make(map[id.RoomID]struct{})
	for _, tup := range tups {
		roomIDs[tup.RoomID] = struct{}{}
	}

	// Now get unique server names from those rooms
	serverNames := make(map[string]struct{})
	for roomID := range roomIDs {
		servers, err := e.db.Rooms.GetCurrentRoomServers(e.ctx, roomID)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if server != e.config.ServerName {
				// We only care about non-local servers
				serverNames[server] = struct{}{}
			}
		}
	}

	if len(serverNames) == 0 {
		return nil
	}

	// Finally send the change
	servers := make([]string, 0, len(serverNames))
	for server := range serverNames {
		servers = append(servers, server)
	}

	e.notifiers.Rooms.SendChange(notifier.Change{
		Servers: servers,
	})

	e.log.Debug().Strs("servers", servers).Msg("Notified federation senders")
	return nil
}
