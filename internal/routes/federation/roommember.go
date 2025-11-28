package federation

import (
	"encoding/json"
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
	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	req.Event.RoomVersion = req.RoomVersion
	req.Event.ID = util.EventIDFromRequestURLParam(r, "eventID")

	// Verify the event ID and signature
	verifyErr, err := util.VerifyEvent(r.Context(), req.Event, middleware.GetRequestServer(r), f.keyStore)
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

	// Store the event as an outlier membership, meaning we index it only for the
	// target of the invite not the room (if any) itself. If this server is in the
	// room already we'll get the full event over federation which will overwrite.
	if err := f.db.Rooms.SendFederatedOutlierMembershipEvent(r.Context(), req.Event); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
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
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipJoin, func(roomID id.RoomID) (any, error) {
		// We need to return the state *before* the new join event, so get that now
		// before we sent the join. Wasteful if the join fails, possible DDOS risk.
		stateWithAuthChain, err := f.db.Rooms.GetCurrentRoomStateEventsWithAuthChain(r.Context(), roomID)
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
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipKnock, func(roomID id.RoomID) (any, error) {
		strippedState, err := f.db.Rooms.GetCurrentRoomStrippedStateEvents(r.Context(), roomID)
		if err != nil {
			return nil, err
		}
		return struct {
			KnockState []*types.Event `json:"knock_room_state"`
		}{strippedState}, nil
	})
}
