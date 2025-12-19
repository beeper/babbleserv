package client

import (
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"

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
	fedClient  *federation.Client
	keyStore   *util.KeyStore
	datastores *util.Datastores
	notifiers  *notifier.Notifiers
}

func NewClientRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	fclient fclient.FederationClient,
	fedClient *federation.Client,
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
		fedClient:  fedClient,
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
	rtr.MethodFunc(http.MethodGet, "/versions", c.GetVersions)
	rtr.MethodFunc(http.MethodGet, "/v3/capabilities", middleware.RequireUserAuth(c.GetCapabilities))

	if c.config.Rooms.Enabled && c.config.Accounts.Enabled && c.config.Transient.Enabled {
		// Legacy (v2/3) sync witb init and increment variants and basic filters, all rooms
		rtr.MethodFunc(http.MethodGet, "/v3/sync", middleware.RequireUserAuth(c.SyncLegacy))
		// Simplified sliding "native" sync MSC4186, same as v2 with room filters, roughly
		rtr.MethodFunc(http.MethodGet, "/unstable/org.matrix.simplified_msc3575/sync", middleware.RequireUserAuth(c.SyncSliding))
		// Beeper's streaming sync, no gaps, firehose style
		rtr.MethodFunc(http.MethodGet, "/unstable/com.beeper.streaming/sync", middleware.RequireUserAuth(c.SyncStreaming))
	}

	if c.config.Rooms.Enabled {
		rtr.MethodFunc(http.MethodPost, "/v3/createRoom", middleware.RequireUserAuth(c.CreateRoom))
		// Send events
		rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}", middleware.RequireUserAuth(c.SendRoomStateEvent))
		rtr.MethodFunc(http.MethodPut, "/v3/rooms/{roomID}/state/{eventType}/", middleware.RequireUserAuth(c.SendRoomStateEvent))
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
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/state/{eventType}", middleware.RequireUserAuth(c.GetRoomStateEvent))
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/state/{eventType}/", middleware.RequireUserAuth(c.GetRoomStateEvent))
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/state/{eventType}/{stateKey}", middleware.RequireUserAuth(c.GetRoomStateEvent))
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/state", middleware.RequireUserAuth(c.GetRoomState))
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/members", middleware.RequireUserAuth(c.GetRoomMembers))

		// Room aliases
		rtr.MethodFunc(http.MethodGet, "/v3/rooms/{roomID}/aliases", middleware.RequireUserAuth(c.GetAliasesForRoom))
		rtr.MethodFunc(http.MethodGet, "/v3/directory/room/{roomAlias}", c.GetAlias)
		rtr.MethodFunc(http.MethodPut, "/v3/directory/room/{roomAlias}", middleware.RequireUserAuth(c.CreateAlias))
		rtr.MethodFunc(http.MethodDelete, "/v3/directory/room/{roomAlias}", middleware.RequireUserAuth(c.DeleteAlias))

		// Profile routes - note the spec has the GET endpoints un-authenticated but Babbleserv disagrees
		rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}", middleware.RequireUserAuth(c.GetProfile))
		rtr.MethodFunc(http.MethodGet, "/v3/profile/{userID}/{key}", middleware.RequireUserAuth(c.GetProfile))
		rtr.MethodFunc(http.MethodPut, "/v3/profile/{userID}/{key}", middleware.RequireUserAuth(c.PutProfile))

		// Receipts routes
		rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/receipt/{receiptType}/{eventID}", middleware.RequireUserAuth(c.SendRoomReadReceipt))
		rtr.MethodFunc(http.MethodPost, "/v3/rooms/{roomID}/read_markers", middleware.RequireUserAuth(c.SendRoomReadMarkers))
	}

	if c.config.Accounts.Enabled {
		rtr.MethodFunc(http.MethodPost, "/v3/register", c.Register)
		rtr.MethodFunc(http.MethodGet, "/v3/login", c.GetLogin)
		rtr.MethodFunc(http.MethodPost, "/v3/login", c.Login)

		rtr.MethodFunc(http.MethodGet, "/v3/whoami", middleware.RequireUserAuth(c.GetWhoami))

		rtr.MethodFunc(http.MethodGet, "/v3/devices", middleware.RequireUserAuth(c.GetDevices))
		rtr.MethodFunc(http.MethodGet, "/v3/devices/{deviceID}", middleware.RequireUserAuth(c.GetDevice))
		rtr.MethodFunc(http.MethodPut, "/v3/devices/{deviceID}", middleware.RequireUserAuth(c.PutDevice))
		rtr.MethodFunc(http.MethodDelete, "/v3/devices/{deviceID}", middleware.RequireUserAuth(c.DeleteDevice))
		rtr.MethodFunc(http.MethodDelete, "/v3/delete_devices", middleware.RequireUserAuth(c.DeleteDevices))

		rtr.MethodFunc(http.MethodGet, "/v3/keys/changes", middleware.RequireUserAuth(c.GetKeyChanges))
		rtr.MethodFunc(http.MethodPost, "/v3/keys/query", middleware.RequireUserAuth(c.QueryKeys))
		rtr.MethodFunc(http.MethodPost, "/v3/keys/upload", middleware.RequireUserAuth(c.UploadKeys))
		rtr.MethodFunc(http.MethodPost, "/v3/keys/claim", middleware.RequireUserAuth(c.ClaimKeys))
		rtr.MethodFunc(http.MethodPost, "/v3/keys/signatures/upload", middleware.RequireUserAuth(c.UploadSignatures))
		rtr.MethodFunc(http.MethodPost, "/v3/keys/device_signing/upload", middleware.RequireUserAuth(c.UploadCrossSigningKeys))

		rtr.MethodFunc(http.MethodPost, "/v3/user/{userID}/filter", middleware.RequireUserAuth(c.CreateFilter))
		rtr.MethodFunc(http.MethodGet, "/v3/user/{userID}/filter/{filterID}", middleware.RequireUserAuth(c.GetFilter))

		// Global account data
		rtr.MethodFunc(http.MethodPut, "/v3/user/{userID}/account_data/{type}", middleware.RequireUserAuth(c.SetAccountData))
		rtr.MethodFunc(http.MethodGet, "/v3/user/{userID}/account_data/{type}", middleware.RequireUserAuth(c.GetAccountData))
		// Room account data
		rtr.MethodFunc(http.MethodPut, "/v3/user/{userID}/rooms/{roomID}/account_data/{type}", middleware.RequireUserAuth(c.SetAccountData))
		rtr.MethodFunc(http.MethodGet, "/v3/user/{userID}/rooms/{roomID}/account_data/{type}", middleware.RequireUserAuth(c.GetAccountData))
	}

	if c.config.Transient.Enabled {
		rtr.MethodFunc(http.MethodPut, "/v3/sendToDevice/{eventType}/{txnID}", middleware.RequireUserAuth(c.SendToDevice))
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
		"versions":          []string{"v1.11"},
		"unstable_features": map[string]any{},
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3capabilities
func (f *ClientRoutes) GetCapabilities(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]any{
		"capabilities": mautrix.RespCapabilities{
			ChangePassword: &mautrix.CapBooleanTrue{},
			RoomVersions: &mautrix.CapRoomVersions{
				Default: "11",
				Available: map[string]mautrix.CapRoomVersionStability{
					"11": mautrix.CapRoomVersionStable,
				},
			},
		},
	})
}
