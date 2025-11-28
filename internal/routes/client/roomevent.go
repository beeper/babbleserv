package client

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/go-chi/chi/v5"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3roomsroomideventeventid
func (c *ClientRoutes) GetRoomEvent(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	eventID := util.EventIDFromRequestURLParam(r, "eventID")
	userID := middleware.GetRequestUserID(r)

	// Check the user *was* joined to the room at the time this event was sent
	if wasInRoom, err := c.db.Rooms.WasUserJoinedRoomAtEvent(r.Context(), userID, roomID, eventID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !wasInRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You do not have access to this event")
		return
	}

	ev, err := c.db.Rooms.GetEvent(r.Context(), eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ev == nil || ev.Rejected || ev.SoftFailed {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if ev.RoomID != roomID {
		// Return a 404 if the event isn't in the room in the request
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EventForClientAPI(ev))
}

// https://spec.matrix.org/v1.14/client-server-api/#get_matrixclientv3roomsroomidstateeventtypestatekey
func (c *ClientRoutes) GetRoomStateEvent(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	userID := middleware.GetRequestUserID(r)

	evType := chi.URLParam(r, "eventType")
	stateKey := chi.URLParam(r, "stateKey")

	// Check the user is currently in the room, state is always available irrespective of send time
	if inRoom, err := c.db.Rooms.IsUserJoinedRoom(r.Context(), userID, roomID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !inRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You are not in this room")
		return
	}

	stateEv, err := c.db.Rooms.GetRoomStateEvent(r.Context(), roomID, types.StateTup{
		Type:     event.NewEventType(evType),
		StateKey: stateKey,
	})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if stateEv == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, stateEv.Content)

}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3roomsroomidstate
func (c *ClientRoutes) GetRoomState(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	userID := middleware.GetRequestUserID(r)

	// Check the user is currently in the room, state is always available irrespective of send time
	if inRoom, err := c.db.Rooms.IsUserJoinedRoom(r.Context(), userID, roomID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !inRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You are not in this room")
		return
	}

	stateEvs, err := c.db.Rooms.GetCurrentRoomStateEvents(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EventsForClientAPI(stateEvs))
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3roomsroomidmembers
func (c *ClientRoutes) GetRoomMembers(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	userID := middleware.GetRequestUserID(r)

	// As above, check the user is in the room now, state is always available to joined members
	if inRoom, err := c.db.Rooms.IsUserJoinedRoom(r.Context(), userID, roomID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !inRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You are not in this room")
		return
	}

	memberEvs, err := c.db.Rooms.GetCurrentRoomMemberEvents(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EventsForClientAPI(memberEvs))
}
