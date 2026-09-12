package notifier

import (
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
)

type Notifiers struct {
	Rooms     *Notifier
	Accounts  *Notifier
	Transient *Notifier
}

func NewNotifiers(cfg config.BabbleConfig, logger zerolog.Logger) *Notifiers {
	log := logger.With().
		Str("component", "notifier").
		Logger()

	return &Notifiers{
		Rooms:     NewNotifier("rooms", cfg.Rooms.Notifier, log),
		Accounts:  NewNotifier("accounts", cfg.Accounts.Notifier, log),
		Transient: NewNotifier("accounts", cfg.Transient.Notifier, log),
	}
}

// Subscribe to changes and get a channel of those. Subscriptions are lossy - if the channel isn't
// being read from we only keep the first change. This means notifier subscriptions can be used to
// wake up systems in response to changes but not to accurately track all changes.
func (n *Notifiers) Subscribe(req Subscription) chan Change {
	// Buffer 1 change to immediately wakeup subscribers if they're currently processing
	return n.SubscribeWithChannel(make(chan Change, 1), req)
}

// Similar to subscribe but takes a custom channel, which can have a greater buffer than default 1,
// but note the delivery is still lossy - if the channel is full the chnage will be dropped.
func (n *Notifiers) SubscribeWithChannel(ch chan Change, req Subscription) chan Change {
	n.Rooms.subscribe(ch, req)
	n.Accounts.subscribe(ch, req)
	n.Transient.subscribe(ch, req)
	return ch
}

func (n *Notifiers) Unsubscribe(ch chan Change) {
	n.Rooms.unsubscribe(ch)
	n.Accounts.unsubscribe(ch)
	n.Transient.unsubscribe(ch)
	close(ch)
}

func (n *Notifiers) Start() {
	n.Rooms.Start()
	n.Accounts.Start()
	n.Transient.Start()
}

func (n *Notifiers) Stop() {
	n.Rooms.Stop()
	n.Accounts.Stop()
	n.Transient.Stop()
}
