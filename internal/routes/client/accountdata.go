package client

import (
	"encoding/json"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/go-chi/chi/v5"

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
