package federation

import (
	"encoding/json"
	"errors"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type inviteRequest struct {
	Event       *types.Event   `json:"event"`
	InviteState []*types.Event `json:"invite_room_state"`
	RoomVersion string         `json:"room_version"`
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
	if res, err := f.db.SendFederatedEvents(r.Context(), roomID, []*types.Event{req.Event}, nil); err != nil {
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
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipLeave, nil)
}

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1make_joinroomiduserid
func (f *FederationRoutes) MakeJoin(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipJoin, true)
}

// https://spec.matrix.org/v1.11/server-server-api/#put_matrixfederationv2send_joinroomideventid
func (f *FederationRoutes) SendJoin(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipJoin, func(roomID id.RoomID, eventID id.EventID) (any, error) {
		// The state before the join. A join stored before without state, as a prev event's, takes the
		// room's current state.
		stateWithAuthChain, err := f.db.Rooms.GetRoomStateWithAuthChainAtEvent(r.Context(), roomID, eventID)
		if errors.Is(err, types.ErrStateUnavailable) {
			stateWithAuthChain, err = f.db.Rooms.GetCurrentRoomStateWithAuthChain(r.Context(), roomID)
		}
		if err != nil {
			return nil, err
		}
		return struct {
			AuthChain []*types.Event `json:"auth_chain"`
			State     []*types.Event `json:"state"`
		}{stateWithAuthChain.AuthChain, stateWithAuthChain.StateEvents}, nil
	})
}

// https://spec.matrix.org/v1.14/server-server-api/#get_matrixfederationv1make_knockroomiduserid
func (f *FederationRoutes) MakeKnock(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipKnock, true)
}

// https://spec.matrix.org/v1.14/server-server-api/#put_matrixfederationv1send_knockroomideventid
func (f *FederationRoutes) SendKnock(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipKnock, func(roomID id.RoomID, _ id.EventID) (any, error) {
		strippedState, err := f.db.Rooms.RoomStrippedState(r.Context(), roomID)
		if err != nil {
			return nil, err
		}
		return struct {
			KnockState []*types.PartialEvent `json:"knock_room_state"`
		}{strippedState}, nil
	})
}
