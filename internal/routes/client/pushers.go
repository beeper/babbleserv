package client

import (
	"encoding/json"
	"net/http"
	"net/url"
	"unicode/utf8"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/pushrules/pushgateway"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

type reqSetPusher struct {
	pushgateway.Pusher
	Append bool `json:"append,omitempty"`
}

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
	const maxPusherBytes = 64 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxPusherBytes)
	body, respErr := util.ParseRequestJSON[json.RawMessage](r)
	if respErr != nil {
		if respErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, respErr)
		} else {
			util.ResponseErrorJSON(w, r, *respErr)
		}
		return
	}
	var req reqSetPusher
	if err := json.Unmarshal(body, &req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	if _, ok := fields["kind"]; !ok {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing kind")
		return
	}

	if req.AppID == "" || req.PushKey == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing app_id or pushkey")
		return
	} else if utf8.RuneCountInString(string(req.AppID)) > 64 || len(req.PushKey) > 512 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "app_id or pushkey is too long")
		return
	}

	userID := middleware.GetRequestUserID(r)

	// A null kind deletes only this user's exact (app_id, pushkey) pair.
	if req.Kind == nil {
		if err := c.db.Accounts.DeletePusherForUser(r.Context(), userID, req.AppID, req.PushKey); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	} else {
		if *req.Kind != pushgateway.PusherKindHTTP {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Unsupported pusher kind")
			return
		}
		if req.AppDisplayName == "" || req.DeviceDisplayName == "" || req.Language == "" || req.Data == nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing app_display_name, device_display_name, lang or data")
			return
		}
		pushURL, err := url.Parse(req.Data.URL())
		if err != nil || pushURL.Scheme != "https" || pushURL.Hostname() == "" || pushURL.Path != "/_matrix/push/v1/notify" {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid HTTPS push gateway URL")
			return
		}
		encoded, err := json.Marshal(&req.Pusher)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		if len(encoded) > maxPusherBytes {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, &mautrix.MTooLarge)
			return
		}
		if err := c.db.Accounts.SetPusherForUser(r.Context(), userID, middleware.GetRequestDeviceID(r), &req.Pusher, req.Append); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
