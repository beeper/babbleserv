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

	var notifiers Notifiers

	if cfg.Rooms.Enabled {
		notifiers.Rooms = NewNotifier("rooms", cfg.Rooms.Notifier, log)
	}
	if cfg.Accounts.Enabled {
		notifiers.Accounts = NewNotifier("accounts", cfg.Accounts.Notifier, log)
	}
	if cfg.Transient.Enabled {
		notifiers.Transient = NewNotifier("accounts", cfg.Transient.Notifier, log)
	}

	return &notifiers
}

func (n *Notifiers) Subscribe(ch chan any, req Subscription) {
	if n.Rooms != nil {
		n.Rooms.Subscribe(ch, req)
	}
	if n.Accounts != nil {
		n.Accounts.Subscribe(ch, req)
	}
	if n.Transient != nil {
		n.Transient.Subscribe(ch, req)
	}
}

func (n *Notifiers) Unsubscribe(ch chan any) {
	if n.Rooms != nil {
		n.Rooms.Unsubscribe(ch)
	}
	if n.Accounts != nil {
		n.Accounts.Unsubscribe(ch)
	}
	if n.Transient != nil {
		n.Transient.Unsubscribe(ch)
	}
}

func (n *Notifiers) Start() {
	if n.Rooms != nil {
		n.Rooms.Start()
	}
	if n.Accounts != nil {
		n.Accounts.Start()
	}
	if n.Transient != nil {
		n.Transient.Start()
	}
}

func (n *Notifiers) Stop() {
	if n.Rooms != nil {
		n.Rooms.Stop()
	}
	if n.Accounts != nil {
		n.Accounts.Stop()
	}
	if n.Transient != nil {
		n.Transient.Stop()
	}
}
