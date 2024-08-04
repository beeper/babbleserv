package routes

import (
	"net/http"

	"github.com/beeper/babbleserv/internal/util"
)

func (rt *Routes) WellKnownClient(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, struct {
		Homeserver map[string]string `json:"m.homeserver"`
	}{map[string]string{"base_url": rt.config.WellKnown.Client}})
}

func (rt *Routes) WellKnownServer(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]string{
		"m.server": rt.config.WellKnown.Server,
	})
}
