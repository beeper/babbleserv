package client

import (
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	defaultUserDirectoryLimit          = 10
	maxUserDirectoryLimit              = 100
	maxUserDirectoryIndexCandidates    = 500
	maxUserDirectoryIndexRows          = 2000
	maxUserDirectoryMembershipRows     = 4096
	maxUserDirectoryMembershipRowsUser = 256
)

type userDirectorySearchRequest struct {
	SearchTerm *string `json:"search_term"`
	Limit      *int    `json:"limit,omitempty"`
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3user_directorysearch
func (c *ClientRoutes) SearchUserDirectory(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[userDirectorySearchRequest](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	if req.SearchTerm == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing search_term")
		return
	}
	limit := defaultUserDirectoryLimit
	if req.Limit != nil {
		limit = *req.Limit
	}
	if limit < 0 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "limit must not be negative")
		return
	} else if limit > maxUserDirectoryLimit {
		limit = maxUserDirectoryLimit
	}
	if *req.SearchTerm == "" || limit == 0 {
		util.ResponseJSON(w, r, http.StatusOK, &types.UserDirectoryResponse{
			Results: []*types.UserDirectoryCandidate{},
		})
		return
	}

	candidates, indexLimited, err := c.db.Accounts.SearchUserDirectoryCandidates(
		r.Context(), *req.SearchTerm, maxUserDirectoryIndexCandidates, maxUserDirectoryIndexRows,
	)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	visible, visibilityLimited, err := c.db.Rooms.FilterUserDirectoryCandidates(
		r.Context(), middleware.GetRequestUserID(r), candidates, limit,
		maxUserDirectoryMembershipRows, maxUserDirectoryMembershipRowsUser,
	)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, &types.UserDirectoryResponse{
		Limited: indexLimited || visibilityLimited,
		Results: visible,
	})
}
