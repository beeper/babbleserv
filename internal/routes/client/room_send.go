package client

import (
	"encoding/json"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/go-chi/chi/v5"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func handleResults(w http.ResponseWriter, r *http.Request, ev *types.Event, eventMap map[id.EventID]error) {
	if err, found := eventMap[ev.ID]; !found {
		panic("input event missing from results")
	} else if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, map[string]id.EventID{
			"event_id": ev.ID,
		})
	}
}

func (c *ClientRoutes) SendRoomStateEvent(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))
	evType := event.NewEventType(chi.URLParam(r, "eventType"))
	stateKey := chi.URLParam(r, "stateKey")

	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUser(r).UserID("localhost")
	ev := types.NewEvent(roomID, evType, &stateKey, userID, content)

	if results, err := c.db.Rooms.SendLocalEvents(r.Context(), roomID, []*types.Event{ev}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		handleResults(w, r, ev, results.EventMap())
	}
}

func (c *ClientRoutes) SendRoomEvent(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))
	evType := event.NewEventType(chi.URLParam(r, "eventType"))

	// TODO: transaction IDs and idempotency
	// Note: asking server to store TXNID indefinitely is stupid (or even beyond like a day)

	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUser(r).UserID("localhost")
	ev := types.NewEvent(roomID, evType, nil, userID, content)

	if results, err := c.db.Rooms.SendLocalEvents(r.Context(), roomID, []*types.Event{ev}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		handleResults(w, r, ev, results.EventMap())
	}
}
