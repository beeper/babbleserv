package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3useruseridfilter
func (c *ClientRoutes) CreateFilter(w http.ResponseWriter, r *http.Request) {
	// Check userID in path is ours
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "UserID mismatch")
		return
	}

	// Leave room below FoundationDB's value limit, including unknown fields.
	const maxFilterBytes = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFilterBytes+1))
	if err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	} else if len(body) > maxFilterBytes {
		util.ResponseErrorMessageJSON(w, r, mautrix.MTooLarge, "Filter exceeds 64 KiB")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	req, respErr := util.ParseRequestJSON[json.RawMessage](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	var filter mautrix.Filter
	if err := json.Unmarshal(req, &filter); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	if filter.EventFormat != "" && filter.EventFormat != mautrix.EventFormatClient && filter.EventFormat != mautrix.EventFormatFederation {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid event_format")
		return
	}
	parts := []*mautrix.FilterPart{filter.AccountData, filter.Presence, filter.BeeperToDevice}
	if filter.Room != nil {
		parts = append(parts, filter.Room.AccountData, filter.Room.Ephemeral, filter.Room.State, filter.Room.Timeline,
			&mautrix.FilterPart{Rooms: filter.Room.Rooms, NotRooms: filter.Room.NotRooms})
	}
	for _, part := range parts {
		if part == nil {
			continue
		}
		if part.Limit < 0 {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Filter limits must be non-negative")
			return
		}
		for _, roomIDs := range [][]id.RoomID{part.Rooms, part.NotRooms} {
			for _, roomID := range roomIDs {
				if _, err := spec.NewRoomID(roomID.String()); err != nil {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid room ID in filter")
					return
				}
			}
		}
		for _, userIDs := range [][]id.UserID{part.Senders, part.NotSenders} {
			for _, userID := range userIDs {
				if _, _, err := userID.ParseAndValidateRelaxed(); err != nil {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid sender ID in filter")
					return
				}
			}
		}
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
	// Check userID in path is ours.
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "UserID mismatch")
		return
	}

	filterID := chi.URLParam(r, "filterID")
	b, err := util.Base64DecodeURLSafe(filterID)
	if err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	filter, err := c.db.Accounts.GetFilter(r.Context(), userID, b)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if filter == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, filter)
}
