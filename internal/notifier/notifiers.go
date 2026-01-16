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

func (n *Notifiers) Subscribe(req Subscription) chan any {
	return n.SubscribeWithChannel(make(chan any, 1), req)
}

func (n *Notifiers) SubscribeWithChannel(ch chan any, req Subscription) chan any {
	n.Rooms.subscribe(ch, req)
	n.Accounts.subscribe(ch, req)
	n.Transient.subscribe(ch, req)
	return ch
}

func (n *Notifiers) Unsubscribe(ch chan any) {
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
