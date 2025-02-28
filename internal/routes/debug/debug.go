package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

type DebugRoutes struct {
	log        zerolog.Logger
	config     config.BabbleConfig
	db         *databases.Databases
	notifiers  *notifier.Notifiers
	datastores *util.Datastores
}

func NewDebugRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	datastores *util.Datastores,
) *DebugRoutes {
	log := log.With().
		Str("routes", "babbleserv").
		Logger()

	return &DebugRoutes{
		log:        log,
		config:     cfg,
		db:         db,
		notifiers:  notifiers,
		datastores: datastores,
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
	rtr.MethodFunc(http.MethodGet, "/debug/user/{userID}/init", b.DebugInitUser)

	rtr.MethodFunc(http.MethodGet, "/debug/server/{serverName}", b.DebugGetServer)
	rtr.MethodFunc(http.MethodGet, "/debug/server/{serverName}/sync", b.DebugSyncServer)

	rtr.MethodFunc(http.MethodGet, "/debug/scratch", b.DebugScratch)
}

func (b *DebugRoutes) DebugScratch(w http.ResponseWriter, r *http.Request) {
	// Scratch debug area

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
