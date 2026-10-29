package federation

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/ingest"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type respGetEvent struct {
	Origin          string         `json:"origin"`
	OriginTimestamp int64          `json:"origin_server_ts"`
	PDUs            []*types.Event `json:"pdus"`
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1eventeventid
//
// Rejected events are served, as Synapse does: a server fetching one a state DAG event cites rejects
// it itself.
func (f *FederationRoutes) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := util.EventIDFromRequestURLParam(r, "eventID")
	ev, err := f.db.Rooms.GetEvent(r.Context(), eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if ev == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if !f.requireServerInRoom(w, r, ev.RoomID) {
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, respGetEvent{
		Origin:          f.config.ServerName,
		OriginTimestamp: time.Now().UnixMilli(),
		PDUs:            []*types.Event{ev},
	})
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1event_authroomideventid
func (f *FederationRoutes) GetEventAuth(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	eventID := util.EventIDFromRequestURLParam(r, "eventID")
	if !f.requireServerInRoom(w, r, roomID) {
		return
	}
	authChain, err := f.db.Rooms.GetEventAuthChain(r.Context(), roomID, eventID)
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

// Past this a state DAG walk answers with fewer events, which the requester walks on from
const maxStateDAGMissingEventsBytes = 8 << 20

// https://spec.matrix.org/v1.10/server-server-api/#post_matrixfederationv1get_missing_eventsroomid
func (f *FederationRoutes) GetMissingEvents(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	req, respErr := util.ParseRequestJSON[ingest.ReqGetMissingEvents](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	} else if !f.requireServerInRoom(w, r, roomID) {
		return
	}

	if req.Limit <= 0 {
		req.Limit = 10
	}

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	stateDAGs := room != nil && util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs)
	if stateDAGs && req.StateDAG {
		f.getMissingStateDAGEvents(w, r, roomID, req)
		return
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
		} else if ev != nil && ev.RoomID == roomID {
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
		// A state DAG room's events cite no auth events, and the state DAG is walked on request
		cited := ev.PrevEventIDs
		if !stateDAGs {
			cited = slices.Concat(ev.PrevEventIDs, ev.AuthEventIDs)
		}
		for _, evID := range cited {
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

func (f *FederationRoutes) getMissingStateDAGEvents(w http.ResponseWriter, r *http.Request, roomID id.RoomID, req ingest.ReqGetMissingEvents) {
	if len(req.EarliestEvents) > ingest.MaxStateDAGRequestIDs || len(req.LatestEvents) > ingest.MaxStateDAGRequestIDs {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Sprintf(
			"A state DAG walk takes at most %d earliest and latest events", ingest.MaxStateDAGRequestIDs,
		))
		return
	}
	evs, err := f.db.Rooms.GetMissingStateDAGEvents(r.Context(), roomID, req.EarliestEvents, req.LatestEvents, min(req.Limit, ingest.MaxStateDAGMissingEvents))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	pdus := util.EventsToMauPDUs(evs)
	size := 0
	for i, pdu := range pdus {
		if size += len(pdu); size > maxStateDAGMissingEventsBytes && i > 0 {
			pdus = pdus[:i]
			break
		}
	}
	util.ResponseJSON(w, r, http.StatusOK, federation.RespGetMissingEvents{Events: pdus})
}

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1backfillroomid
func (f *FederationRoutes) BackfillEvents(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
