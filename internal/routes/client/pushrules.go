package client

import (
	"net/http"

	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) GetPushRules(w http.ResponseWriter, r *http.Request) {
	// Just a stub implementation
	util.ResponseJSON(w, r, http.StatusOK, struct {
		Global map[string]any `json:"global"`
	}{map[string]any{}})
}
