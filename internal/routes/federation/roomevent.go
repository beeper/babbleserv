package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1eventeventid
func (f *FederationRoutes) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "eventID")
	// TODO: check server is in room?
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
	// TODO: check server is in room?
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

// https://spec.matrix.org/v1.10/server-server-api/#post_matrixfederationv1get_missing_eventsroomid
func (f *FederationRoutes) GetMissingEvents(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[federation.ReqGetMissingEvents](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if req.Limit == 0 {
		req.Limit = 10
	}

	// We search up to these
	seenEvents := make(map[id.EventID]struct{}, len(req.EarliestEvents))
	for _, evid := range req.EarliestEvents {
		seenEvents[evid] = struct{}{}
	}

	// From these
	latestEvents := make(map[id.EventID]struct{}, len(req.LatestEvents))
	for _, evid := range req.LatestEvents {
		latestEvents[evid] = struct{}{}
	}

	evs := make([]*types.Event, 0, len(req.LatestEvents))
	completeEvs := make([]*types.Event, 0, req.Limit)

	handleEventID := func(evID id.EventID) error {
		if _, found := seenEvents[evID]; found {
			return nil
		}
		ev, err := f.db.Rooms.GetEvent(r.Context(), evID)
		if err != nil {
			return err
		} else if ev != nil {
			evs = append(evs, ev)
			seenEvents[ev.ID] = struct{}{}
			// Include if *not* one of the latestEventIDs provided
			if _, ok := latestEvents[ev.ID]; !ok {
				completeEvs = append(completeEvs, ev)
			}
		}
		return nil
	}

	// Seed evs with the latest events
	for _, evID := range req.LatestEvents {
		handleEventID(evID)
	}

	var ev *types.Event
	for len(evs) > 0 && len(completeEvs) < req.Limit {
		// Pop the first event, check each of it's prev and auth events
		ev, evs = evs[0], evs[1:]
		for _, evID := range append(ev.PrevEventIDs, ev.AuthEventIDs...) {
			if err := handleEventID(evID); err != nil {
				hlog.FromRequest(r).Err(err).
					Str("room_id", ev.RoomID.String()).
					Str("event_id", ev.ID.String()).
					Msg("Error handling missing event")
			}
		}
	}

	// Now re-sort the events since we appended prev events after each other
	util.SortEventList(completeEvs)
	util.ResponseJSON(w, r, http.StatusOK, federation.RespGetMissingEvents{
		Events: util.EventsToMauPDUs(completeEvs),
	})
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1backfillroomid
func (f *FederationRoutes) BackfillEvents(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
