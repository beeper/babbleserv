package internal

import (
	"net/http"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/routes"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/workers"
)

type Babbleserv struct {
	db       *databases.Databases
	notifier *notifier.Notifier
	routes   *routes.Routes
	workers  *workers.Workers
}

type UserAgentTransport struct {
	rt http.RoundTripper
	ua string
}

func (t *UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Add("User-Agent", t.ua)
	return t.rt.RoundTrip(req)
}

func NewBabbleserv(cfg config.BabbleConfig) *Babbleserv {
	log := log.With().Logger()

	// Overwrite default Go HTTP client user agent
	http.DefaultClient.Transport = &UserAgentTransport{http.DefaultTransport, cfg.UserAgent}

	// Create a global federation client
	keyID, key := cfg.MustGetActiveSigningKey()
	fclient := fclient.NewFederationClient([]*fclient.SigningIdentity{{
		ServerName: spec.ServerName(cfg.ServerName),
		KeyID:      gomatrixserverlib.KeyID(keyID),
		PrivateKey: key,
	}}, fclient.WithUserAgent(cfg.UserAgent), fclient.WithSkipVerify(true))

	// Create a global key store to cache server signing keys
	keyStore := util.NewKeyStore(fclient)

	// Create the notifier instance
	notif := notifier.NewNotifier(cfg, log)

	db := databases.NewDatabases(cfg, log, notif)

	var rts *routes.Routes
	if cfg.RoutesEnabled {
		rts = routes.NewRoutes(cfg, log, db, notif, fclient, keyStore)
	} else {
		log.Info().Msg("Routes disabled")
	}

	var wrks *workers.Workers
	if cfg.WorkersEnabled {
		wrks = workers.NewWorkers(cfg, log, db, notif, fclient)
	} else {
		log.Info().Msg("Workers disabled")
	}

	return &Babbleserv{
		db:       db,
		notifier: notif,
		routes:   rts,
		workers:  wrks,
	}
}

func (b *Babbleserv) Start() {
	b.db.Start()
	b.notifier.Start()

	if b.routes != nil {
		b.routes.Start()
	}
	if b.workers != nil {
		b.workers.Start()
	}
}

func (b *Babbleserv) Stop() {
	if b.routes != nil {
		b.routes.Stop()
	}
	if b.workers != nil {
		b.workers.Stop()
	}

	b.notifier.Stop()
	b.db.Stop()
}
