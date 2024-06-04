package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1eventeventid
func (f *FederationRoutes) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "eventID")
	ev, err := f.db.Rooms.GetEvent(r.Context(), id.EventID(eventID))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, ev)
	return
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1event_authroomideventid
func (f *FederationRoutes) GetEventAuth(w http.ResponseWriter, r *http.Request) {
	// roomID := chi.URLParam(r, "roomID") // don't actually need it!
	eventID := chi.URLParam(r, "eventID")
	authChain, err := f.db.Rooms.GetEventAuthChain(r.Context(), id.EventID(eventID))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, authChain)
	return
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1stateroomid
func (f *FederationRoutes) GetState(w http.ResponseWriter, r *http.Request) {
	// TODO: is this ALL state or just auth state (please be this, probably all)
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1state_idsroomid
func (f *FederationRoutes) GetStateIDs(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
