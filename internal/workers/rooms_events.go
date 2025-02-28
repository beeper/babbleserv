package workers

import (
	"context"
	"sync"
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
	eventsIteratorLockName    = "EventsIteratorLock"
	eventsIteratorLockRetry   = time.Second * 5
	eventsIteratorLockTimeout = time.Second * 10
	eventsIteratorBatchSize   = 10
)

// The events iterator is a singleton background worker that iterates over all
// events ever stored by Babbleserv and triggers other things. Currently this
// is just waking up federation senders.
type EventsIterator struct {
	log       zerolog.Logger
	config    config.BabbleConfig
	db        *databases.Databases
	notifiers *notifier.Notifiers

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func NewEventsIterator(
	logger zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *EventsIterator {
	log := logger.With().
		Str("worker", "EventsIterator").
		Logger()

	return &EventsIterator{
		log:       log,
		config:    cfg,
		db:        db,
		notifiers: notifiers,
	}
}

func (ei *EventsIterator) Start() {
	ei.ctx, ei.cancel = context.WithCancel(ei.log.WithContext(context.Background()))

	ei.wg.Add(1)
	go func() {
		defer ei.wg.Done()
		lock.WithLock(ei.ctx, ei.db.Rooms, eventsIteratorLockName, lock.LockOptions{
			RetryInterval: eventsIteratorLockRetry,
			Timeout:       eventsIteratorLockTimeout,
		}, ei.handleNewEventsLoop)
	}()
}

func (ei *EventsIterator) Stop() {
	ei.cancel()
	ei.wg.Wait()
	ei.log.Info().Msg("Events iterator stopped")
}

func (ei *EventsIterator) handleNewEventsLoop(lock lock.Lock) {
	newEventsCh := make(chan any, 1)
	ei.notifiers.Rooms.Subscribe(newEventsCh, notifier.Subscription{AllEvents: true})
	defer ei.notifiers.Rooms.Unsubscribe(newEventsCh)

	// Cold start case: handle anything waiting right away
	ei.handleNewEvents(lock)

	for {
		select {
		case <-ei.ctx.Done():
			lock.Release()
			return
		case <-newEventsCh:
			ei.handleNewEvents(lock)
		case <-time.After(eventsIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (ei *EventsIterator) handleNewEvents(lock lock.Lock) {
	startVersion, err := ei.db.Rooms.GetEventsIteratorPosition(ei.ctx)
	if err != nil {
		ei.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh the lock before we process each batch
		lock.Refresh()

		// Bump the user version - we store the position of the most recent event
		// and the ranges are inclusive of that.
		newEventIDTups, err := ei.db.Rooms.EventsIteratorPaginateEvents(ei.ctx, currentVersion, eventsIteratorBatchSize)
		if err != nil {
			ei.log.Err(err).Msg("Failed to paginate new events")
			return
		} else if len(newEventIDTups) == 0 {
			ei.log.Trace().Any("fromVersion", currentVersion).Msg("No events found")
			break
		}

		ei.log.Info().
			Int("events", len(newEventIDTups)).
			Any("fromVersion", currentVersion).
			Msg("Handling new events batch")

		if err := ei.notifyFederationSenders(newEventIDTups); err != nil {
			ei.log.Err(err).Msg("Failed to notify federation senders")
		}

		currentVersion = newEventIDTups[len(newEventIDTups)-1].Version
		currentVersion.UserVersion += 1

		if len(newEventIDTups) < eventsIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = ei.db.Rooms.UpdateEventsIteratorPosition(ei.ctx, currentVersion, lock.TxnRefresh)
	if err != nil {
		ei.log.Err(err).Msg("Failed to update current position")
		return
	}
}

func (ei *EventsIterator) notifyFederationSenders(tups []types.EventIDTupWithVersion) error {
	// Get unique room IDs
	roomIDs := make(map[id.RoomID]struct{})
	for _, tup := range tups {
		roomIDs[tup.RoomID] = struct{}{}
	}

	// Now get unique server names from those rooms
	serverNames := make(map[string]struct{})
	for roomID := range roomIDs {
		servers, err := ei.db.Rooms.GetCurrentRoomServers(ei.ctx, roomID)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if server != ei.config.ServerName {
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

	ei.notifiers.Rooms.SendChange(notifier.Change{
		Servers: servers,
	})
	return nil
}
