package internal

import (
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/routes"
)

type Babbleserv struct {
	db     *databases.Databases
	routes *routes.Routes
}

func NewBabbleserv(cfg config.BabbleConfig) *Babbleserv {
	log := log.With().Logger()

	db := databases.NewDatabases(cfg, log)
	routes := routes.NewRoutes(cfg, log, db)

	return &Babbleserv{
		db:     db,
		routes: routes,
	}
}

func (b *Babbleserv) Start() {
	b.routes.Start()
}

func (b *Babbleserv) Stop() {
	b.routes.Stop()
}
