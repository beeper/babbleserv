package client

import (
	"net/http"

	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) Sync(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
