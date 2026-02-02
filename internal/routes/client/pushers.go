package client

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/pushrules/pushgateway"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushers
func (c *ClientRoutes) GetPushers(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)

	pushers, err := c.db.Accounts.GetPushersForUser(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, pushgateway.RespPushers{Pushers: pushers})
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3pushersset
func (c *ClientRoutes) SetPusher(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[pushgateway.Pusher](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if req.PushKey == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing pushkey")
		return
	}

	userID := middleware.GetRequestUserID(r)

	// Per spec, if kind is null/empty, delete the pusher
	if req.Kind == nil {
		if err := c.db.Accounts.DeletePusherForUser(r.Context(), userID, req.PushKey); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	} else {
		if req.AppDisplayName == "" || req.AppID == "" || req.Data == nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing app_display_name, app_id or data")
			return
		}
		if err := c.db.Accounts.SetPusherForUser(r.Context(), userID, &req); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
