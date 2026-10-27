package federation

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// The invite room state, stripped state or with MSC4311 whole events, is not read, so it is left
// unparsed: a stripped create event is no PDU to check
type inviteRequest struct {
	Event       *types.Event      `json:"event"`
	InviteState []json.RawMessage `json:"invite_room_state"`
	RoomVersion string            `json:"room_version"`
}

// https://spec.matrix.org/v1.10/server-server-api/#put_matrixfederationv2inviteroomideventid
func (f *FederationRoutes) SignInvite(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	if roomID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	} else if req.Event == nil || req.Event.Type != event.StateMember || req.Event.StateKey == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Event is not a member event")
		return
	} else if req.Event.Membership() != event.MembershipInvite {
		// Stored as an outlier, unauthorized, when this server is not in the room
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Membership is not invite")
		return
	} else if id.UserID(*req.Event.StateKey).Homeserver() != f.config.ServerName {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "The invite event must be for this server")
		return
	} else if req.Event.RoomID != roomID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Event room ID does not match the request")
		return
	} else if !f.config.JoinsRoomVersion(req.RoomVersion) {
		responseIncompatibleRoomVersion(w, r, req.RoomVersion, "Room version not supported")
		return
	}

	req.Event.RoomVersion = req.RoomVersion
	req.Event.ID = util.EventIDFromRequestURLParam(r, "eventID")

	// Verify the event ID and signature
	verifyErr, err := util.VerifyEventFromServer(r.Context(), req.Event, middleware.GetRequestServer(r), f.keyStore)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if verifyErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
		return
	}

	keyID, key := f.config.MustGetActiveSigningKey()
	signature, err := util.GetEventSignature(req.Event, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	req.Event.Signatures[f.config.ServerName] = map[string]string{
		keyID: signature,
	}

	// For a room this server is not in, the invite is stored as an outlier, so the target local user
	// sees it in sync without it forming part of the room. Otherwise it is sent as any federated event,
	// and received a second time over federation as a duplicate.
	if res, err := f.db.SendFederatedEvents(r.Context(), roomID, []*types.Event{req.Event}, nil, rooms.SendFederatedEventsOptions{}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(res.Rejected) > 0 {
		util.ResponseRejectedEventJSON(w, r, res.Rejected[0].Error)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Event *types.Event `json:"event"`
	}{req.Event})
}

// https://spec.matrix.org/v1.14/server-server-api/#get_matrixfederationv1make_leaveroomiduserid
func (f *FederationRoutes) MakeLeave(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipLeave, false)
}

// https://spec.matrix.org/v1.14/server-server-api/#put_matrixfederationv2send_leaveroomideventid
func (f *FederationRoutes) SendLeave(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipLeave, membershipResponse{})
}

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1make_joinroomiduserid
func (f *FederationRoutes) MakeJoin(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipJoin, true)
}

// https://spec.matrix.org/v1.11/server-server-api/#put_matrixfederationv2send_joinroomideventid
func (f *FederationRoutes) SendJoin(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipJoin, membershipResponse{
		// A state DAG too large to serve with the join refuses the join before it is sent, rather
		// than leave the joiner joined here alone. A join stored already counts in it, so is answered
		// again as the first was.
		beforeSend: func(room *types.Room, ev *types.Event) error {
			if !util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) {
				return nil
			} else if stored, err := f.db.Rooms.GetStoredEventIDs(r.Context(), []id.EventID{ev.ID}); err != nil || len(stored) > 0 {
				return err
			}
			return f.db.Rooms.CheckServedStateDAG(r.Context(), room.ID)
		},
		afterSend: func(room *types.Room, ev *types.Event) (any, error) {
			if util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) {
				return stateDAGJoinResponse{f: f, room: room, ev: ev}, nil
			}
			// The state before the join. A join stored before without state, as a prev event's,
			// takes the room's current state.
			stateWithAuthChain, err := f.db.Rooms.GetRoomStateWithAuthChainAtEvent(r.Context(), room.ID, ev.ID)
			if errors.Is(err, types.ErrStateUnavailable) {
				stateWithAuthChain, err = f.db.Rooms.GetCurrentRoomStateWithAuthChain(r.Context(), room.ID)
			}
			if err != nil {
				return nil, err
			}
			return struct {
				AuthChain []*types.Event `json:"auth_chain"`
				State     []*types.Event `json:"state"`
			}{stateWithAuthChain.AuthChain, stateWithAuthChain.StateEvents}, nil
		},
	})
}

// stateDAGJoinResponse is the send_join response of a state DAG room (MSC4242), whose state DAG
// replaces the state before the join, its auth chain and the servers in the room:
// {origin, event, state_dag}. It is written as the state DAG is read, a page a transaction, as a large
// room's is far more than one transaction reads or a response should hold whole.
type stateDAGJoinResponse struct {
	f    *FederationRoutes
	room *types.Room
	ev   *types.Event
}

// write streams the response. Should reading the state DAG fail part way, the body is left
// unfinished, so the joining server takes it for a failed request and sends the join again.
func (resp stateDAGJoinResponse) write(w http.ResponseWriter, r *http.Request) {
	log := hlog.FromRequest(r)
	head, err := json.Marshal(struct {
		Origin string       `json:"origin"`
		Event  *types.Event `json:"event"`
	}{resp.f.config.ServerName, resp.ev})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := bufio.NewWriter(w)
	body.Write(head[:len(head)-1])
	body.WriteString(`,"state_dag":[`)
	served := 0
	err = resp.f.db.Rooms.StreamServedStateDAG(r.Context(), resp.room.ID, func(evs []*types.Event) error {
		for _, ev := range evs {
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			} else if served > 0 {
				body.WriteByte(',')
			}
			body.Write(b)
			served++
		}
		return body.Flush()
	})
	if err != nil {
		log.Err(err).Int("served", served).Msg("Failed to serve the state DAG of a join")
		return
	}
	body.WriteString("]}")
	if err := body.Flush(); err != nil {
		log.Warn().Err(err).Msg("Failed to finish serving the state DAG of a join")
	}
	log.Debug().Int("state_dag", served).Msg("Served the state DAG of a join")
}

// https://spec.matrix.org/v1.14/server-server-api/#get_matrixfederationv1make_knockroomiduserid
func (f *FederationRoutes) MakeKnock(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipKnock, true)
}

// https://spec.matrix.org/v1.14/server-server-api/#put_matrixfederationv1send_knockroomideventid
func (f *FederationRoutes) SendKnock(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipKnock, membershipResponse{
		afterSend: func(room *types.Room, _ *types.Event) (any, error) {
			strippedState, err := f.db.Rooms.RoomStrippedState(r.Context(), room.ID)
			if err != nil {
				return nil, err
			}
			return struct {
				KnockState []*types.PartialEvent `json:"knock_room_state"`
			}{strippedState}, nil
		},
	})
}
