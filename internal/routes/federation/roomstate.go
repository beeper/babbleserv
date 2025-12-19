package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1stateroomid
func (f *FederationRoutes) GetState(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	eventID := r.URL.Query().Get("event_id")
	if eventID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing event ID")
		return
	}
	state, err := f.db.Rooms.GetRoomStateWithAuthChainAtEvent(r.Context(), id.RoomID(roomID), id.EventID(eventID))
	if err == types.ErrEventNotFound {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Event not found")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, struct {
		State     []*types.Event `json:"pdus"`
		AuthChain []*types.Event `json:"auth_chain"`
	}{state.StateEvents, state.AuthChain})
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1state_idsroomid
func (f *FederationRoutes) GetStateIDs(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	eventID := r.URL.Query().Get("event_id")
	if eventID == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing event ID")
		return
	}
	stateIDs, err := f.db.Rooms.GetRoomStateWithAuthChainIDsAtEvent(r.Context(), id.RoomID(roomID), id.EventID(eventID))
	if err == types.ErrEventNotFound {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Event not found")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, struct {
		State     []id.EventID `json:"pdu_ids"`
		AuthChain []id.EventID `json:"auth_chain_ids"`
	}{stateIDs.StateEventIDs, stateIDs.AuthChainIDs})
}
