package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/federation"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

type FederationRoutes struct {
	log        zerolog.Logger
	db         *databases.Databases
	config     config.BabbleConfig
	fclient    fclient.FederationClient
	fedclient  *federation.Client
	keyStore   *util.KeyStore
	datastores *util.Datastores
	notifiers  *notifier.Notifiers
}

func NewFederationRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	fclient fclient.FederationClient,
	fedclient *federation.Client,
	keyStore *util.KeyStore,
	datastores *util.Datastores,
	notifiers *notifier.Notifiers,
) *FederationRoutes {
	log := log.With().
		Str("routes", "federation").
		Logger()

	return &FederationRoutes{
		log:        log,
		db:         db,
		config:     cfg,
		fclient:    fclient,
		fedclient:  fedclient,
		keyStore:   keyStore,
		datastores: datastores,
		notifiers:  notifiers,
	}
}

func (f *FederationRoutes) AddKeyRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v2/server", f.GetKeys)
	rtr.MethodFunc(http.MethodPost, "/v2/query", f.QueryKeys)
	rtr.MethodFunc(http.MethodPost, "/v2/query/{serverName}", f.QueryKey)
}

func (f *FederationRoutes) AddFederationRoutes(rtr chi.Router) {
	rtr.MethodFunc(http.MethodGet, "/v1/version", f.GetVersion)

	requireServerAuth := middleware.NewServerAuthMiddleware(f.config.ServerName, f.keyStore)

	if f.config.Rooms.Enabled {
		rtr.MethodFunc(http.MethodPut, "/v1/send/{txnID}", requireServerAuth(f.SendTransaction))

		rtr.MethodFunc(http.MethodGet, "/v1/event/{eventID}", requireServerAuth(f.GetEvent))
		rtr.MethodFunc(http.MethodGet, "/v1/event_auth/{roomID}/{eventID}", requireServerAuth(f.GetEventAuth))
		rtr.MethodFunc(http.MethodPost, "/v1/get_missing_events/{roomID}", requireServerAuth(f.GetMissingEvents))

		rtr.MethodFunc(http.MethodGet, "/v1/state/{roomID}", requireServerAuth(f.GetState))
		rtr.MethodFunc(http.MethodGet, "/v1/state_ids/{roomID}", requireServerAuth(f.GetStateIDs))

		rtr.MethodFunc(http.MethodPut, "/v2/invite/{roomID}/{eventID}", requireServerAuth(f.SignInvite))

		rtr.MethodFunc(http.MethodGet, "/v1/make_join/{roomID}/{userID}", requireServerAuth(f.MakeJoin))
		rtr.MethodFunc(http.MethodGet, "/v1/make_leave/{roomID}/{userID}", requireServerAuth(f.MakeLeave))
		rtr.MethodFunc(http.MethodGet, "/v1/make_knock/{roomID}/{userID}", requireServerAuth(f.MakeKnock))

		rtr.MethodFunc(http.MethodPut, "/v2/send_join/{roomID}/{eventID}", requireServerAuth(f.SendJoin))
		rtr.MethodFunc(http.MethodPut, "/v2/send_leave/{roomID}/{eventID}", requireServerAuth(f.SendLeave))
		rtr.MethodFunc(http.MethodPut, "/v1/send_knock/{roomID}/{eventID}", requireServerAuth(f.SendKnock))
	}

	if f.config.Accounts.Enabled {
		rtr.MethodFunc(http.MethodGet, "/v1/user/devices/{userID}", requireServerAuth(f.GetUserDevices))

		rtr.MethodFunc(http.MethodGet, "/v1/query/profile", requireServerAuth(f.QueryProfile))

		rtr.MethodFunc(http.MethodPost, "/v1/user/keys/query", requireServerAuth(f.QueryUserKeys))
		rtr.MethodFunc(http.MethodPost, "/v1/user/keys/claim", requireServerAuth(f.ClaimUserKeys))
	}

	if f.config.Transient.Enabled {

	}

	if f.config.Media.Enabled {
		rtr.MethodFunc(http.MethodGet, "/v1/media/download/{mediaID}", requireServerAuth(f.DownloadMedia))
		rtr.MethodFunc(http.MethodGet, "/v1/media/thumbnail/{mediaID}", requireServerAuth(f.DownloadThumbnail))
	}
}

func (f *FederationRoutes) GetVersion(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]string{
		"name": "Babbleserv",
	})
}
