package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1eventeventid
func (f *FederationRoutes) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "eventID")
	ev, err := f.db.Rooms.GetEvent(r.Context(), id.EventID(eventID))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ev == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, ev)
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1event_authroomideventid
func (f *FederationRoutes) GetEventAuth(w http.ResponseWriter, r *http.Request) {
	// roomID := chi.URLParam(r, "roomID") // don't actually need it!
	eventID := chi.URLParam(r, "eventID")
	authChain, err := f.db.Rooms.GetEventAuthChain(r.Context(), id.EventID(eventID))
	if err == types.ErrEventNotFound {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Event not found")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, struct {
		AuthChain []*types.Event `json:"auth_chain"`
	}{authChain})
}

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

// https://spec.matrix.org/v1.10/server-server-api/#post_matrixfederationv1get_missing_eventsroomid
func (f *FederationRoutes) GetMissingEvents(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1backfillroomid
func (f *FederationRoutes) BackfillEvents(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
