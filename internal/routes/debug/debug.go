package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
)

type DebugRoutes struct {
	log      zerolog.Logger
	config   config.BabbleConfig
	db       *databases.Databases
	notifier *notifier.Notifier
}

func NewDebugRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	notifier *notifier.Notifier,
) *DebugRoutes {
	log := log.With().
		Str("routes", "babbleserv").
		Logger()

	return &DebugRoutes{
		log:      log,
		config:   cfg,
		db:       db,
		notifier: notifier,
	}
}

func (b *DebugRoutes) AddDebugRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/debug/event/{eventID}", b.DebugGetEvent)
	rtr.MethodFunc(http.MethodPost, "/debug/events/{roomID}", b.DebugMakeEvents)

	rtr.MethodFunc(http.MethodGet, "/debug/room/{roomID}", b.DebugGetRoom)
	rtr.MethodFunc(http.MethodGet, "/debug/room/{roomID}/state/{eventID}", b.DebugGetRoomStateAt)

	rtr.MethodFunc(http.MethodPost, "/debug/notifier/change", b.DebugSendNotifierChange)

	rtr.MethodFunc(http.MethodGet, "/debug/user/{userID}", b.DebugGetUser)
	rtr.MethodFunc(http.MethodGet, "/debug/user/{userID}/sync", b.DebugSyncUser)

	rtr.MethodFunc(http.MethodGet, "/debug/server/{serverName}", b.DebugGetServer)
	rtr.MethodFunc(http.MethodGet, "/debug/server/{serverName}/sync", b.DebugSyncServer)
}
