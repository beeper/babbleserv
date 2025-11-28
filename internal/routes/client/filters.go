package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3useruseridfilter
func (c *ClientRoutes) CreateFilter(w http.ResponseWriter, r *http.Request) {
	// Check userID in path is ours
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Username mismatch")
		return
	}

	req, respErr := util.ParseRequestJSON[mautrix.Filter](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	filterID, err := c.db.Accounts.CreateFilter(r.Context(), userID, req)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespCreateFilter{
		FilterID: util.Base64EncodeURLSafe(filterID),
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3useruseridfilterfilterid
func (c *ClientRoutes) GetFilter(w http.ResponseWriter, r *http.Request) {
	// Check userID in path is ours - is this correct? Spec unclear.
	// TODO: check - can users dl other users filters? (seems wrong)
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Username mismatch")
		return
	}

	filterID := chi.URLParam(r, "filterID")
	b, err := util.Base64DecodeURLSafe(filterID)
	if err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	}

	filter, err := c.db.Accounts.GetFilter(r.Context(), userID, b)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, filter)
}
