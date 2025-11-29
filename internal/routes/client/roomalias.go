package client

import (
	"errors"
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3roomsroomidaliases
func (c *ClientRoutes) GetAliasesForRoom(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	userID := middleware.GetRequestUserID(r)

	// Check the user is currently in the room
	if inRoom, err := c.db.Rooms.IsUserJoinedRoom(r.Context(), userID, roomID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if !inRoom {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "You are not in this room")
		return
	}

	aliases, err := c.db.Rooms.GetAliasesForRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, map[string]any{
		"aliases": aliases,
	})
}

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3directoryroomroomalias
// note: this route is *not* guarded behind authentication
func (c *ClientRoutes) GetAlias(w http.ResponseWriter, r *http.Request) {
	roomAlias := util.RoomAliasFromRequestURLParam(r, "roomAlias")

	roomID, err := c.db.Rooms.GetRoomAlias(r.Context(), roomAlias)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if roomID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room alias not found")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespAliasResolve{
		RoomID:  roomID,
		Servers: []string{c.config.ServerName},
	})
}

// https://spec.matrix.org/v1.16/client-server-api/#put_matrixclientv3directoryroomroomalias
func (c *ClientRoutes) CreateAlias(w http.ResponseWriter, r *http.Request) {
	roomAlias := util.RoomAliasFromRequestURLParam(r, "roomAlias")
	userID := middleware.GetRequestUserID(r)

	var req mautrix.ReqAliasCreate

	req, respErr := util.ParseRequestJSON[mautrix.ReqAliasCreate](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	} else if req.RoomID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing RoomID")
		return
	} else if util.HomeserverForRoomID(req.RoomID) != c.config.ServerName {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Refusing to set alias for nonlocal room")
		return
	}

	err := c.db.Rooms.SetRoomAlias(r.Context(), roomAlias, req.RoomID, userID)
	if errors.Is(err, types.ErrRoomAliasTaken) {
		util.ResponseErrorJSON(w, r, mautrix.MRoomInUse)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#delete_matrixclientv3directoryroomroomalias
func (c *ClientRoutes) DeleteAlias(w http.ResponseWriter, r *http.Request) {
	roomAlias := util.RoomAliasFromRequestURLParam(r, "roomAlias")
	userID := middleware.GetRequestUserID(r)

	err := c.db.Rooms.DeleteRoomAlias(r.Context(), roomAlias, userID)
	if errors.Is(err, types.ErrRoomAliasNotFound) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room alias not found")
		return
	} else if err != nil {
		// Check if it's a permission error (cannot delete another user's alias)
		if err.Error() == "cannot delete another users alias" {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
			return
		}
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
