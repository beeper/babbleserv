package client

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3presenceuseridstatus
func (c *ClientRoutes) GetPresence(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	presence, err := c.db.Transient.GetUserPresence(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	if presence == nil {
		// Return unavailable if we don't have presence data
		util.ResponseJSON(w, r, http.StatusOK, mautrix.RespPresence{
			Presence: "unavailable",
		})
		return
	}

	resp := mautrix.RespPresence{
		Presence:  presence.Presence,
		StatusMsg: presence.Message,
	}

	// Calculate last active time in milliseconds
	if !presence.LastActive.IsZero() {
		lastActiveAgo := time.Since(presence.LastActive).Milliseconds()
		resp.LastActiveAgo = int(lastActiveAgo)
		resp.CurrentlyActive = presence.Presence == event.PresenceOnline
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3presenceuseridstatus
func (c *ClientRoutes) PutPresence(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqPresence](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	userID := middleware.GetRequestUserID(r)
	userIDParam := id.UserID(chi.URLParam(r, "userID"))
	if userIDParam != userID {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	}

	// Validate presence state
	if req.Presence != "online" && req.Presence != "offline" && req.Presence != "unavailable" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid presence state")
		return
	}

	presence := &types.Presence{
		UserID:     userID,
		Presence:   req.Presence,
		Message:    req.StatusMsg,
		LastActive: time.Now(),
	}

	if err := c.db.Transient.UpdateUserPresence(r.Context(), userID, presence); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct{}{})
}
