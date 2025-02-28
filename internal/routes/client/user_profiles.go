package client

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuserid
// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuseridavatar_url
// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuseriddisplayname
func (c *ClientRoutes) GetProfile(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	profile, err := c.db.Rooms.GetUserProfile(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if profile == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var resp any = profile

	key := chi.URLParam(r, "key")
	if key == "displayname" {
		resp = map[string]string{
			"displayname": profile.DisplayName,
		}
	} else if key == "avatar_url" {
		resp = map[string]string{
			"avatar_url": profile.AvatarURL,
		}
	} else if key != "" {
		resp = map[string]any{
			key: profile.Custom[key],
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.10/client-server-api/#put_matrixclientv3profileuseridavatar_url
// https://spec.matrix.org/v1.10/client-server-api/#put_matrixclientv3profileuseriddisplayname
func (c *ClientRoutes) PutProfile(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUserID(r)
	userIDParam := chi.URLParam(r, "userID")
	if userIDParam != userID.String() {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	value, found := req[key]
	if !found {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing value")
		return
	}

	if err := c.db.Rooms.UpdateUserProfile(
		r.Context(), userID, key, value,
	); err != nil && err != types.ErrProfileNotChanged {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct{}{})
}
