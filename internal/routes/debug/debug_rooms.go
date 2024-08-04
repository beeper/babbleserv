package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugGetRoom(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))

	room, err := b.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	servers, err := b.db.Rooms.GetCurrentRoomServers(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	stateEvs, err := b.db.Rooms.GetCurrentRoomStateEvents(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	extremIDs, err := b.db.Rooms.GetRoomCurrentExtremEventIDs(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Room           *types.Room         `json:"room"`
		Servers        []string            `json:"servers"`
		ExtremEventIDs []id.EventID        `json:"extreme_event_ids"`
		StateEvents    []types.ClientEvent `json:"current_state"`
	}{room, servers, extremIDs, util.EventsToClientEvents(stateEvs)})
}

func (b *DebugRoutes) DebugGetRoomStateAt(w http.ResponseWriter, r *http.Request) {
	roomID := id.RoomID(chi.URLParam(r, "roomID"))
	eventID := id.EventID(chi.URLParam(r, "eventID"))

	allStateMap, err := b.db.Rooms.GetRoomStateMapAtEvent(r.Context(), roomID, eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	userIDs := make([]id.UserID, 0)
	for stateTup := range allStateMap {
		if stateTup.Type == event.StateMember {
			userIDs = append(userIDs, id.UserID(stateTup.StateKey))
		}
	}

	authStateMap, err := b.db.Rooms.GetRoomAuthStateMapAtEvent(r.Context(), roomID, eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	authStateMapOK := true
	for stateTup, evID := range authStateMap {
		if otherEvID, found := allStateMap[stateTup]; !found || otherEvID != evID {
			authStateMapOK = false
		}
	}

	memberStateMap, err := b.db.Rooms.GetRoomSpecificRoomMemberStateMapAtEvent(r.Context(), roomID, userIDs, eventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	memberStateMapOK := true
	for stateTup, evID := range memberStateMap {
		if otherEvID, found := allStateMap[stateTup]; !found || otherEvID != evID {
			memberStateMapOK = false
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		State         types.StateMap `json:"all_state"`
		AuthStateOK   bool           `json:"auth_state_ok"`
		AuthState     types.StateMap `json:"auth_state"`
		MemberStateOK bool           `json:"member_state_ok"`
		MemberState   types.StateMap `json:"member_state"`
	}{allStateMap, authStateMapOK, authStateMap, memberStateMapOK, memberStateMap})
}
