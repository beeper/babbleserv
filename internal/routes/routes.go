package routes

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/routes/client"
	"github.com/beeper/babbleserv/internal/routes/federation"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/libserv/pkg/requestlog"
)

type Routes struct {
	log        zerolog.Logger
	client     *client.ClientRoutes
	federation *federation.FederationRoutes
	servers    []*Server
}

type Server struct {
	http.Server
	ServiceGroups []string
}

func NewRoutes(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	databases *databases.Databases,
) *Routes {
	log := logger.With().
		Str("component", "routes").
		Logger()

	r := &Routes{
		log:        log,
		client:     client.NewClientRoutes(cfg, logger, databases),
		federation: federation.NewFederationRoutes(cfg, logger, databases),
		servers:    make([]*Server, 0),
	}

	for _, serverCfg := range cfg.Servers {
		server := &Server{
			Server: http.Server{
				Addr:    serverCfg.ListenAddr,
				Handler: r.MakeHandler(serverCfg.ServiceGroups),
			},
			ServiceGroups: serverCfg.ServiceGroups,
		}
		r.servers = append(r.servers, server)
	}

	return r
}

func (r *Routes) MakeHandler(groups []string) http.Handler {
	// Create base router w/recovery & logging
	rtr := chi.NewRouter()
	rtr.Use(hlog.NewHandler(r.log))
	rtr.Use(hlog.RequestIDHandler("request_id", ""))
	rtr.Use(requestlog.AccessLogger(true))
	rtr.Use(middleware.NewRecoveryMiddleware(r.log))
	rtr.Use(middleware.AuthMiddleware)

	for _, group := range groups {
		switch group {
		case "client":
			rtr.Route("/_matrix/client", r.client.AddClientRoutes)
		case "federation":
			rtr.Route("/_matrix/key", r.federation.AddKeyRoutes)
			rtr.Route("/_matrix/federation", r.federation.AddFederationRoutes)
		default:
			panic(fmt.Errorf("Invalid route group: %s", group))
		}
	}

	rtr.NotFound(func(w http.ResponseWriter, r *http.Request) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
	})
	rtr.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		util.ResponseErrorJSON(w, r, util.MMethodNotAllowed)
	})

	return rtr
}

func (r *Routes) Start() {
	r.log.Info().Msg("Starting servers...")

	for _, server := range r.servers {
		go func() {
			r.log.Info().Msgf(
				"Start listen on: %s (groups=%s)",
				server.Addr,
				server.ServiceGroups,
			)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				r.log.Panic().Err(err).Msg("Error in server listener")
			}
		}()
	}
}

func (r *Routes) Stop() {
	r.log.Info().Msg("Shutdown initiated...")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	for _, server := range r.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Shutdown(ctx); err != nil {
				r.log.Panic().Err(err).Msg("Server shutdown failed")
			}
		}()
	}

	wg.Wait()
	r.log.Info().Msg("Servers stopped")
}
