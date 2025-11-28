package debug

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugGetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := id.EventID(chi.URLParam(r, "eventID"))

	ev, err := b.db.Rooms.GetEvent(r.Context(), eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ev == nil {
		util.ResponseJSON(w, r, http.StatusOK, struct {
			Event *struct{} `json:"event"`
		}{nil})
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Event      *types.Event `json:"event"`
		SoftFailed bool         `json:"soft_failed"`
		Outlier    bool         `json:"outlier"`
	}{util.EventForClientAPI(ev), ev.SoftFailed, ev.Outlier})
}

func (b *DebugRoutes) DebugMakeEvents(w http.ResponseWriter, r *http.Request) {
	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	query := r.URL.Query()

	count := 1
	if query.Has("count") {
		countStr := query.Get("count")
		var err error
		count, err = strconv.Atoi(countStr)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	stateKeys := query["state_key"]
	evTypes := query["type"]
	userIDs := query["user_id"]

	if len(evTypes) != count || len(userIDs) != count {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MInvalidParam,
			"One of type or user_id query parameters do not have enough values as count",
		)
		return
	}

	roomID := id.RoomID(chi.URLParam(r, "roomID"))

	partialEvs := make([]*types.PartialEvent, 0, count)
	for i := range count {
		var sKey *string = nil
		if len(stateKeys) > i {
			sKey = &stateKeys[i]
		}
		partialEvs = append(partialEvs,
			types.NewPartialEvent(
				roomID,
				event.NewEventType(evTypes[i]),
				sKey,
				id.UserID(userIDs[i]),
				content,
			),
		)
	}

	evs, rejected, err := b.db.Rooms.PrepareLocalEvents(r.Context(), roomID, partialEvs)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(rejected) > 0 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, rejected[0].Error.Error())
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Events []*types.Event `json:"events"`
	}{evs})
}
