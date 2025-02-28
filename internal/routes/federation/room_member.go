package federation

import (
	"encoding/json"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/databases/rooms"
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

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1make_joinroomiduserid
func (f *FederationRoutes) MakeJoin(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	userID := util.UserIDFromRequestURLParam(r, "userID")

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	} else {
		var hasVer bool
		for _, ver := range r.URL.Query()["ver"] {
			if ver == room.Version {
				hasVer = true
				break
			}
		}
		if !hasVer {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Room version not supported")
			return
		}
	}

	// content := map[string]any{
	// 	"membership": event.MembershipJoin,
	// }

	// Grab the remote users profile to populate displayname/avatarurl in the
	// join event, log/ignore any error.
	// if profileResp, err := f.fclient.LookupProfile(
	// 	r.Context(),
	// 	spec.ServerName(f.config.ServerName),
	// 	spec.ServerName(userID.Homeserver()),
	// 	userID.String(),
	// 	"",
	// ); err != nil {
	// 	hlog.FromRequest(r).
	// 		Warn().
	// 		Err(err).
	// 		Msg("Error fetching profile information when making remote join")
	// } else {
	// 	if profileResp.DisplayName != "" {
	// 		content["displayname"] = profileResp.DisplayName
	// 	}
	// 	if profileResp.AvatarURL != "" {
	// 		content["avatar_url"] = profileResp.AvatarURL
	// 	}
	// }

	sKey := userID.String()
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, map[string]any{
		"membership": event.MembershipJoin,
	})

	evs, evErr, err := f.db.Rooms.PrepareLocalEvents(r.Context(), []*types.PartialEvent{partialEv})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if evErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, evErr.Error())
		return
	}
	ev := evs[0]

	// Drop the signatures/hashes - the joining server may alter the content
	// before they call send_join and these should not be included.
	// ev.Hashes = make(map[string]string)
	clear(ev.Hashes)
	clear(ev.Signatures)

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Event       *types.Event `json:"event"`
		RoomVersion string       `json:"room_version"`
	}{ev, ev.RoomVersion})
}

// https://spec.matrix.org/v1.11/server-server-api/#put_matrixfederationv2send_joinroomideventid
func (f *FederationRoutes) SendJoin(w http.ResponseWriter, r *http.Request) {
	var ev types.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	ev.RoomID = roomID
	ev.ID = util.EventIDFromRequestURLParam(r, "eventID")

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	} else {
		ev.RoomVersion = room.Version
	}

	verifyErr, err := util.VerifyEvent(r.Context(), &ev, middleware.GetRequestServer(r), f.keyStore)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if verifyErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
		return
	}

	// We need to return the state *before* the new join event, so get that now
	// before we sent the join. Wasteful if the join fails, possible DDOS risk.
	stateWithAuthChain, err := f.db.Rooms.GetCurrentRoomStateEventsWithAuthChain(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	options := rooms.SendFederatedEventsOptions{}
	results, err := f.db.Rooms.SendFederatedEvents(r.Context(), roomID, []*types.Event{&ev}, options)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(results.Rejected) > 0 {
		err := results.Rejected[0].Error
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Origin    string         `json:"string"`
		AuthChain []*types.Event `json:"auth_chain"`
		State     []*types.Event `json:"state"`
	}{f.config.ServerName, stateWithAuthChain.AuthChain, stateWithAuthChain.StateEvents})
}
