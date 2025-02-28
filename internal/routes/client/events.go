package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) GetRoomEvent(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))
	eventID := id.EventID(chi.URLParam(r, "eventID"))

	userID := middleware.GetRequestUserID(r)
	if inRoom, err := c.db.Rooms.IsUserInRoom(r.Context(), userID, roomID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !inRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You are not in this room")
		return
	}

	ev, err := c.db.Rooms.GetEvent(r.Context(), eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ev == nil || ev.RoomID != roomID {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, ev.ClientEvent())
}

func (c *ClientRoutes) GetRoomState(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))

	userID := middleware.GetRequestUserID(r)
	if inRoom, err := c.db.Rooms.IsUserInRoom(r.Context(), userID, roomID); err != nil {
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

	util.ResponseJSON(w, r, http.StatusOK, util.EventsToClientEvents(stateEvs))
}

func (c *ClientRoutes) GetRoomMembers(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))

	userID := middleware.GetRequestUserID(r)
	if inRoom, err := c.db.Rooms.IsUserInRoom(r.Context(), userID, roomID); err != nil {
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

	util.ResponseJSON(w, r, http.StatusOK, util.EventsToClientEvents(memberEvs))
}
