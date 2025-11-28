package federation

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func sendEventErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, rooms.ErrAuthStage4) {
		// Stage4 errors happen when the events own auth events don't allow it, so this is an
		// invalid input param.
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	// Stage5 errors happen based on the events prev_events (after passing stage4), so should
	// be treated as an authorization error.
	util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
}

func (f *FederationRoutes) makeMembershipEventForOtherServer(
	w http.ResponseWriter,
	r *http.Request,
	membership event.Membership,
	checkRequestVersions bool,
) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	remoteUserID := util.UserIDFromRequestURLParam(r, "userID")

	otherServer := remoteUserID.Homeserver()
	if otherServer == f.config.ServerName {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"UserID is from this server, cannot make federated event",
		)
	} else if otherServer != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"UserID does not match requesting server origin",
		)
	}

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	} else if checkRequestVersions {
		if !slices.Contains(r.URL.Query()["ver"], room.Version) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Room version not supported")
			return
		}
	}

	content := map[string]any{
		"membership": membership,
	}

	// If possible grab the users profile from the requesting server
	// TODO: check if we have the profile locally (uh, where)
	if f.config.Federation.FetchProfileForMemberEvents {
		if profileResp, err := f.fclient.LookupProfile(
			r.Context(),
			spec.ServerName(f.config.ServerName),
			spec.ServerName(otherServer),
			remoteUserID.String(),
			"",
		); err != nil {
			hlog.FromRequest(r).
				Warn().
				Err(err).
				Msg("Error fetching profile information when making remote join")
		} else {
			if profileResp.DisplayName != "" {
				content["displayname"] = profileResp.DisplayName
			}
			if profileResp.AvatarURL != "" {
				content["avatar_url"] = profileResp.AvatarURL
			}
		}
	}

	sKey := remoteUserID.String()
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, remoteUserID, content)

	evs, rejected, err := f.db.Rooms.PrepareLocalEvents(r.Context(), roomID, []*types.PartialEvent{partialEv})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(rejected) > 0 {
		sendEventErrorResponse(w, r, rejected[0].Error)
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

func (f *FederationRoutes) sendMembershipEventFromOtherServer(
	w http.ResponseWriter,
	r *http.Request,
	membership event.Membership,
	getResponseBeforeSend func(id.RoomID) (any, error),
) {
	var ev types.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	if ev.Type != event.StateMember {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Event type is not m.room.member")
		return
	} else if ev.StateKey == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "State key is not set")
		return
	} else if ev.Membership() != membership {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Membership is incorrect")
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

	var response any = util.EmptyJSON
	if getResponseBeforeSend != nil {
		response, err = getResponseBeforeSend(roomID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	options := rooms.SendFederatedEventsOptions{}
	res, err := f.db.Rooms.SendFederatedEvents(r.Context(), roomID, []*types.Event{&ev}, options)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(res.Rejected) > 0 {
		sendEventErrorResponse(w, r, res.Rejected[0].Error)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, response)
}
