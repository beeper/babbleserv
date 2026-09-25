package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/go-chi/chi/v5"

	"github.com/beeper/babbleserv/internal/databases/accounts"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3useruseridaccount_datatype
// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3useruseridroomsroomidaccount_datatype
func (c *ClientRoutes) SetAccountData(w http.ResponseWriter, r *http.Request) {
	// Check userID in path is ours
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Username mismatch")
		return
	}

	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	adType := event.NewEventType(chi.URLParam(r, "type"))

	// Decode + re-encode the content as JSON to ensure validity
	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}
	contentBytes, _ := json.Marshal(content)

	ad := types.AccountData{
		AccountDataTup: types.AccountDataTup{
			UserID: userID,
			RoomID: roomID,
			Type:   adType,
		},
		Content: contentBytes,
	}

	if err := c.db.Accounts.SetAccountData(r.Context(), []*types.AccountData{&ad}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3useruseridaccount_datatype
// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3useruseridroomsroomidaccount_datatype
func (c *ClientRoutes) GetAccountData(w http.ResponseWriter, r *http.Request) {
	// Check userID in path is ours
	pathUserID := util.UserIDFromRequestURLParam(r, "userID")
	userID := middleware.GetRequestUserID(r)
	if userID != pathUserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Username mismatch")
		return
	}

	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	adType := event.NewEventType(chi.URLParam(r, "type"))

	ad, err := c.db.Accounts.GetAccountData(r.Context(), userID, roomID, adType)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ad == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, ad.Content)
}

// Chi matches RawPath when present; otherwise net/http has already decoded
// Path. Decode exactly once, including literal percent escapes in identifiers.
func roomTagURLParam(r *http.Request, name string) string {
	value := chi.URLParam(r, name)
	if r.URL.RawPath != "" {
		value, _ = url.PathUnescape(value)
	}
	return value
}

// https://spec.matrix.org/v1.19/client-server-api/#room-tagging
func (c *ClientRoutes) GetRoomTags(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	if userID.String() != roomTagURLParam(r, "userID") {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	}
	ad, err := c.db.Accounts.GetAccountData(r.Context(), userID, id.RoomID(roomTagURLParam(r, "roomID")), event.AccountDataRoomTags)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	tags := accounts.RoomTagsContent{}
	if ad != nil {
		if err := json.Unmarshal(ad.Content, &tags); err != nil {
			util.ResponseErrorUnknownJSON(w, r, fmt.Errorf("decode stored m.tag content: %w", err))
			return
		}
	}
	if tags.Tags == nil {
		tags.Tags = make(map[string]json.RawMessage)
	}
	util.ResponseJSON(w, r, http.StatusOK, tags)
}

func (c *ClientRoutes) PutRoomTag(w http.ResponseWriter, r *http.Request) {
	c.updateRoomTag(w, r, false)
}

func (c *ClientRoutes) DeleteRoomTag(w http.ResponseWriter, r *http.Request) {
	c.updateRoomTag(w, r, true)
}

func (c *ClientRoutes) updateRoomTag(w http.ResponseWriter, r *http.Request, remove bool) {
	userID := middleware.GetRequestUserID(r)
	if userID.String() != roomTagURLParam(r, "userID") {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	}
	tag := roomTagURLParam(r, "tag")
	if len(tag) == 0 || len(tag) > 255 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Tag name must contain 1 to 255 bytes")
		return
	}
	var encoded json.RawMessage
	if !remove {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, accounts.MaxRoomTagsBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, &mautrix.MTooLarge)
			} else {
				util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
			}
			return
		}
		if !json.Valid(body) {
			util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
			return
		}
		var content map[string]json.RawMessage
		if err := json.Unmarshal(body, &content); err != nil || content == nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
		if raw, ok := content["order"]; ok {
			var order *float64
			if err := json.Unmarshal(raw, &order); err != nil || order == nil || *order < 0 || *order > 1 {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Tag order must be a number between 0 and 1")
				return
			}
		}
		encoded, err = json.Marshal(content)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}
	if err := c.db.Accounts.UpdateRoomTag(r.Context(), userID, id.RoomID(roomTagURLParam(r, "roomID")), tag, encoded, remove); err != nil {
		if errors.Is(err, accounts.ErrRoomTagsTooLarge) {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, &mautrix.MTooLarge)
		} else {
			util.ResponseErrorUnknownJSON(w, r, err)
		}
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
