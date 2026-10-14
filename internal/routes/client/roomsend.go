package client

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/go-chi/chi/v5"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) SendRoomStateEvent(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	evType := util.EventTypeFromRequestURLParam(r, "eventType")
	stateKey := util.StateKeyFromRequestURLParam(r, "stateKey")
	if evType == event.StateCreate {
		util.ResponseErrorMessageJSON(w, r, mautrix.MBadJSON, "Create events can only be sent by createRoom")
		return
	}

	content, respErr := util.ParseRequestJSON[map[string]any](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
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
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	evType := util.EventTypeFromRequestURLParam(r, "eventType")
	txnID := chi.URLParam(r, "txnID")

	content, respErr := util.ParseRequestJSON[map[string]any](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	userID := middleware.GetRequestUserID(r)
	ev := types.NewPartialEvent(roomID, evType, nil, userID, content)
	c.sendLocalEventHandleResultsWithOptions(w, r, roomID, ev, rooms.SendLocalEventsOptions{
		TransactionDevice:   middleware.GetRequestUserDevice(r),
		TransactionEndpoint: "send/" + evType.Type,
		TransactionID:       txnID,
	}, func(ev *types.Event) any {
		return map[string]id.EventID{
			"event_id": ev.ID,
		}
	})
}
