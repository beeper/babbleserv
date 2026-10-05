package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// federation.RespGetState, with each event encoded straight into the response
type respGetState struct {
	AuthChain []*types.Event `json:"auth_chain"`
	PDUs      []*types.Event `json:"pdus"`
}

// requireServerInRoom responds and returns false unless the requesting server is in the room, as
// Synapse refuses a server not joined to the room's current state
func (f *FederationRoutes) requireServerInRoom(w http.ResponseWriter, r *http.Request, roomID id.RoomID) bool {
	if joined, err := f.db.Rooms.IsServerJoined(r.Context(), roomID, middleware.GetRequestServer(r)); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return false
	} else if !joined {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Host not in room")
		return false
	}
	return true
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1stateroomid
func (f *FederationRoutes) GetState(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	eventID := r.URL.Query().Get("event_id")
	if eventID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing event ID")
		return
	} else if !f.requireServerInRoom(w, r, roomID) {
		return
	}
	state, err := f.db.Rooms.GetRoomStateWithAuthChainAtEvent(r.Context(), roomID, id.EventID(eventID))
	if err != nil {
		responseStateAtEventError(w, r, eventID, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, respGetState{
		AuthChain: state.AuthChain,
		PDUs:      state.StateEvents,
	})
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1state_idsroomid
func (f *FederationRoutes) GetStateIDs(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	eventID := r.URL.Query().Get("event_id")
	if eventID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing event ID")
		return
	} else if !f.requireServerInRoom(w, r, roomID) {
		return
	}
	stateIDs, err := f.db.Rooms.GetRoomStateWithAuthChainIDsAtEvent(r.Context(), roomID, id.EventID(eventID))
	if err != nil {
		responseStateAtEventError(w, r, eventID, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, federation.RespGetStateIDs{
		PDUs:      stateIDs.StateEventIDs,
		AuthChain: stateIDs.AuthChainIDs,
	})
}

func responseStateAtEventError(w http.ResponseWriter, r *http.Request, eventID string, err error) {
	// Exact on purpose: a wrapped ErrEventNotFound for a missing state or auth chain PDU stays a 500
	switch err {
	case types.ErrEventNotFound:
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Event not found")
	case types.ErrStateUnavailable:
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "State not known at event "+eventID)
	default:
		util.ResponseErrorUnknownJSON(w, r, err)
	}
}
