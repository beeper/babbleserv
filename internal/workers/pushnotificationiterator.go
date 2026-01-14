package workers

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
)

const (
	pushNotificationIteratorPositionsKey = "PushNotificationIteratorPositions"
	pushNotificationIteratorLockName     = "PushNotificationIteratorLock"
	pushNotificationIteratorLockRetry    = time.Second * 5
	pushNotificationIteratorLockTimeout  = time.Second * 10
	pushNotificationIteratorBatchSize    = 10
)

// The PushNotificationIterator sends push notifications to local user devices for new events.
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
	// n.handler = n.handleNotificationsLoop
	return n
}
