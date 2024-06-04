package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/middleware"
)

type ClientRoutes struct {
	log zerolog.Logger
	db  *databases.Databases
}

func NewClientRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
) *ClientRoutes {
	log := log.With().
		Str("routes", "client").
		Logger()

	return &ClientRoutes{
		log: log,
		db:  db,
	}
}

func (c *ClientRoutes) AddClientRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v3/sync", middleware.RequireUserAuth(c.Sync))

	// Room routes
	rtr.MethodFunc(http.MethodPost, "/v3/createRoom", middleware.RequireUserAuth(c.CreateRoom))
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}", middleware.RequireUserAuth(c.SendRoomStateEvent))
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}/{stateKey}", middleware.RequireUserAuth(c.SendRoomStateEvent))
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/send/{eventType}/{txnID}", middleware.RequireUserAuth(c.SendRoomEvent))
}
