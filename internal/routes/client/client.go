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
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

type ClientRoutes struct {
	backgroundWg sync.WaitGroup

	log        zerolog.Logger
	db         *databases.Databases
	config     config.BabbleConfig
	fclient    fclient.FederationClient
	keyStore   *util.KeyStore
	datastores *util.Datastores
	notifiers  *notifier.Notifiers
}

func NewClientRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	fclient fclient.FederationClient,
	keyStore *util.KeyStore,
	datastores *util.Datastores,
	notifiers *notifier.Notifiers,
) *ClientRoutes {
	log := log.With().
		Str("routes", "client").
		Logger()

	return &ClientRoutes{
		log:        log,
		db:         db,
		config:     cfg,
		fclient:    fclient,
		keyStore:   keyStore,
		datastores: datastores,
		notifiers:  notifiers,
	}
}

func (c *ClientRoutes) Stop() {
	c.log.Debug().Msg("Waiting for any background jobs to complete...")
	c.backgroundWg.Wait()
}

func (c *ClientRoutes) AddClientRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v3/versions", c.GetVersions)

	if c.config.Rooms.Enabled && c.config.Accounts.Enabled && c.config.Transient.Enabled {
		rtr.MethodFunc(http.MethodGet, "/v3/sync", middleware.RequireUserAuth(c.Sync))
	}

	if c.config.Rooms.Enabled {
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

		// Profile routes - note the spec has the GET endpoints un-authenticated but Babbleserv disagrees
		rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}", middleware.RequireUserAuth(c.GetProfile))
		rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}/{key}", middleware.RequireUserAuth(c.GetProfile))
		rtr.MethodFunc(http.MethodPut, "/v3/profile/{userID}/{key}", middleware.RequireUserAuth(c.PutProfile))

		// Receipts routes
		rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/receipt/{receiptType}/{eventID}", c.SendRoomReadReceipt)
		rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/read_markers", c.SendRoomReadMarkers)
	}

	if c.config.Accounts.Enabled {
		rtr.MethodFunc(http.MethodPost, "/v3/register", c.Register)
		rtr.MethodFunc(http.MethodGet, "/v3/login", c.GetLogin)
		rtr.MethodFunc(http.MethodPost, "/v3/login", c.Login)

		rtr.MethodFunc(http.MethodPost, "/v3/keys/upload", middleware.RequireUserAuth(c.UploadKeys))
	}

	if c.config.Transient.Enabled {

	}

	if c.config.Media.Enabled {
		rtr.MethodFunc(http.MethodGet, "/v1/media/config", middleware.RequireUserAuth(c.GetMediaConfig))

		rtr.MethodFunc(http.MethodGet, "/v1/media/download/{serverName}/{mediaID}", middleware.RequireUserAuth(c.DownloadMedia))
		rtr.MethodFunc(http.MethodGet, "/v1/media/download/{serverName}/{mediaID}/{filename}", middleware.RequireUserAuth(c.DownloadMedia))
		rtr.MethodFunc(http.MethodGet, "/v1/media/thumbnail/{serverName}/{mediaID}", middleware.RequireUserAuth(c.DownloadThumbnail))
	}
}

func (c *ClientRoutes) AddClientMediaRoutes(rtr chi.Router) {
	if c.config.Media.Enabled {
		rtr.MethodFunc(http.MethodPost, "/v1/create", middleware.RequireUserAuth(c.CreateMedia))
		rtr.MethodFunc(http.MethodPost, "/v1/complete", middleware.RequireUserAuth(c.CompleteMedia))
		rtr.MethodFunc(http.MethodPost, "/v3/upload", middleware.RequireUserAuth(c.UploadMedia))
		rtr.MethodFunc(http.MethodPut, "/v3/upload/{serverName}/{mediaID}", middleware.RequireUserAuth(c.UploadMedia))
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientversions
func (f *ClientRoutes) GetVersions(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]any{
		"versions":          []string{"1.11"},
		"unstable_features": []string{},
	})
}
