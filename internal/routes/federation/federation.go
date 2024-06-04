package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/util"
)

type FederationRoutes struct {
	log zerolog.Logger
	db  *databases.Databases
}

func NewFederationRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
) *FederationRoutes {
	log := log.With().
		Str("routes", "federation").
		Logger()

	return &FederationRoutes{
		log: log,
		db:  db,
	}
}

func (f *FederationRoutes) AddKeyRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v2/server", f.GetKey)
	rtr.MethodFunc(http.MethodPost, "/v2/query", f.QueryKeys)
	rtr.MethodFunc(http.MethodPost, "/v2/query/{serverName}", f.QueryKey)
}

func (f *FederationRoutes) AddFederationRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v1/version", f.GetVersion)

	// TODO: auth middleware
	rtr.MethodFunc(http.MethodPut, "/v1/send/{tnxID}", f.SendTransaction)

	rtr.MethodFunc(http.MethodGet, "/v1/event/{eventID}", f.GetEvent)
	rtr.MethodFunc(http.MethodGet, "/v1/event_auth/{roomID}/{eventID}", f.GetEventAuth)

	rtr.MethodFunc(http.MethodGet, "/v1/state/{roomID}", f.GetState)
	rtr.MethodFunc(http.MethodGet, "/v1/state_ids/{roomID}", f.GetStateIDs)

}

func (f *FederationRoutes) GetVersion(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]string{
		"name":    "Babbleserv",
		"version": "COMMIT GO HERE",
	})
}
