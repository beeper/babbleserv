package client

import (
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

type ClientRoutes struct {
	backgroundWg sync.WaitGroup

	log      zerolog.Logger
	db       *databases.Databases
	config   config.BabbleConfig
	fclient  fclient.FederationClient
	keyStore *util.KeyStore
}

func NewClientRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	fclient fclient.FederationClient,
	keyStore *util.KeyStore,
) *ClientRoutes {
	log := log.With().
		Str("routes", "client").
		Logger()

	return &ClientRoutes{
		log:      log,
		db:       db,
		config:   cfg,
		fclient:  fclient,
		keyStore: keyStore,
	}
}

func (c *ClientRoutes) Stop() {
	c.log.Debug().Msg("Waiting for any background jobs to complete...")
	c.backgroundWg.Wait()
}

func (c *ClientRoutes) AddClientRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v3/sync", middleware.RequireUserAuth(c.Sync))

	// Rooms
	//
	rtr.MethodFunc(http.MethodPost, "/v3/createRoom", middleware.RequireUserAuth(c.CreateRoom))
	// Send events
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}", middleware.RequireUserAuth(c.SendRoomStateEvent))
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}/{stateKey}", middleware.RequireUserAuth(c.SendRoomStateEvent))
	rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/send/{eventType}/{txnID}", middleware.RequireUserAuth(c.SendRoomEvent))
	// Send membership events
	rtr.MethodFunc(http.MethodGet, "/v3/joined_rooms", middleware.RequireUserAuth(c.GetJoinedRooms))
	rtr.MethodFunc(http.MethodPost, "/v3/join/{roomID}", middleware.RequireUserAuth(c.SendRoomJoinAlias))
	rtr.MethodFunc(http.MethodPost, "/v3/knock/{roomID}", middleware.RequireUserAuth(c.SendRoomKnockAlias))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/invite", middleware.RequireUserAuth(c.SendRoomInvite))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/join", middleware.RequireUserAuth(c.SendRoomJoin))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/forget", middleware.RequireUserAuth(c.ForgetRoom))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/leave", middleware.RequireUserAuth(c.SendRoomLeave))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/kick", middleware.RequireUserAuth(c.SendRoomKick))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/ban", middleware.RequireUserAuth(c.SendRoomBan))
	rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/unban", middleware.RequireUserAuth(c.SendRoomUnban))
	// Get events/state
	rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/event/{eventID}", middleware.RequireUserAuth(c.GetRoomEvent))
	rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/state", middleware.RequireUserAuth(c.GetRoomState))
	rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/members", middleware.RequireUserAuth(c.GetRoomMembers))

	// Profile routes (note GET are not authenticated at all)
	rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}", c.GetProfile)
	rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}/{key}", c.GetProfile)
	rtr.MethodFunc(http.MethodPut, "/v3/profile/{userID}/{key}", middleware.RequireUserAuth(c.PutProfile))
}
