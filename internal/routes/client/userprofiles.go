package client

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuserid
// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuseridavatar_url
// https://spec.matrix.org/v1.10/client-server-api/#get_matrixclientv3profileuseriddisplayname
func (c *ClientRoutes) GetProfile(w http.ResponseWriter, r *http.Request) {
	userID := util.UserIDFromRequestURLParam(r, "userID")

	profile, err := c.db.Accounts.GetUserProfile(r.Context(), userID)
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
	req, respErr := util.ParseRequestJSON[map[string]any](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	userID := middleware.GetRequestUserID(r)
	userIDParam := util.UserIDFromRequestURLParam(r, "userID")
	if userIDParam != userID {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	}

	key := chi.URLParam(r, "key")
	value, found := req[key]
	if !found {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing value")
		return
	}
	if key == "displayname" || key == "avatar_url" {
		if _, ok := value.(string); !ok {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Profile value must be a string")
			return
		}
	}

	if err := c.db.Accounts.UpdateUserProfile(
		r.Context(), userID, key, value,
	); errors.Is(err, types.ErrProfileDisplayNameTooLong) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct{}{})
}
