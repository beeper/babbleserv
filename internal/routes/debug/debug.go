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

func (d *DebugRoutes) AddDebugRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/debug/event/{eventID}", d.DebugGetEvent)
	rtr.MethodFunc(http.MethodPost, "/debug/events/{roomID}", d.DebugMakeEvents)

	rtr.MethodFunc(http.MethodGet, "/debug/room/{roomID}", d.DebugGetRoom)
	rtr.MethodFunc(http.MethodGet, "/debug/room/{roomID}/state/{eventID}", d.DebugGetRoomStateAt)

	rtr.MethodFunc(http.MethodPost, "/debug/notifier/change", d.DebugSendNotifierChange)

	rtr.MethodFunc(http.MethodGet, "/debug/user/{userID}", d.DebugGetUser)
	rtr.MethodFunc(http.MethodGet, "/debug/user/{userID}/sync", d.DebugSyncUser)

	rtr.MethodFunc(http.MethodGet, "/debug/server/{serverName}", d.DebugGetServer)

	rtr.MethodFunc(http.MethodGet, "/debug/system/iterators", d.DebugGetIteratorPositions)

	rtr.MethodFunc(http.MethodGet, "/debug/scratch", d.DebugScratch)
}

func (d *DebugRoutes) DebugScratch(w http.ResponseWriter, r *http.Request) {
	// Scratch debug area

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
