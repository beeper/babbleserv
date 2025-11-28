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

func (c *ClientRoutes) SendRoomStateEvent(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))
	evType := event.NewEventType(chi.URLParam(r, "eventType"))
	stateKey := chi.URLParam(r, "stateKey")

	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUserID(r)
	ev := types.NewPartialEvent(roomID, evType, &stateKey, userID, content)
	c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
		return map[string]id.EventID{
			"event_id": ev.ID,
		}
	})
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

	userID := middleware.GetRequestUserID(r)
	ev := types.NewPartialEvent(roomID, evType, nil, userID, content)
	c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
		return map[string]id.EventID{
			"event_id": ev.ID,
		}
	})
}
