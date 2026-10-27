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

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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
		return
	} else if otherServer != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"UserID does not match requesting server origin",
		)
		return
	}

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	} else if room.LocalMembers == 0 {
		// The matching send_ would be refused, as a room this server left takes no remote user's
		// membership. A rejected invite falls back to the invitee's server recording it locally.
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Not an active room on this server")
		return
	} else if checkRequestVersions && !slices.Contains(r.URL.Query()["ver"], room.Version) {
		responseIncompatibleRoomVersion(w, r, room.Version, "Your homeserver does not support the room's version")
		return
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
		util.ResponseRejectedEventJSON(w, r, rejected[0].Error)
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

// membershipResponse is how a send_ endpoint answers once the event is sent, refusing it beforehand,
// once it is verified, with the error beforeSend returns. A response too large to build whole is a
// streamedResponse.
type membershipResponse struct {
	beforeSend func(room *types.Room, ev *types.Event) error
	afterSend  func(room *types.Room, ev *types.Event) (any, error)
}

// streamedResponse is a response written as it is read
type streamedResponse interface {
	write(w http.ResponseWriter, r *http.Request)
}

func (f *FederationRoutes) sendMembershipEventFromOtherServer(
	w http.ResponseWriter,
	r *http.Request,
	membership event.Membership,
	response membershipResponse,
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
	} else if ev.Sender.Homeserver() != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "User not from origin")
		return
	} else if *ev.StateKey != ev.Sender.String() {
		// As Synapse, before authorizing it: no kicks or bans
		util.ResponseErrorMessageJSON(w, r, mautrix.MBadJSON, "state_key and sender must match")
		return
	}

	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	ev.RoomID = roomID
	ev.ID = util.EventIDFromRequestURLParam(r, "eventID")
	// Event was generated by us, and will be sent over federation
	ev.Local = true

	room, err := f.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	}
	ev.RoomVersion = room.Version

	// Veriy the event is signed by the requesting HS
	verifyErr, err := util.VerifyEvent(r.Context(), &ev, f.keyStore)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if verifyErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
		return
	}
	if response.beforeSend != nil {
		if err := response.beforeSend(room, &ev); err != nil {
			responseMembershipError(w, r, err)
			return
		}
	}

	// For a room this server is not in, none is taken, as a remote user's membership is never an
	// outlier
	res, err := f.db.SendFederatedEvents(r.Context(), roomID, []*types.Event{&ev}, nil, rooms.SendFederatedEventsOptions{})
	var rejection error
	if err == nil && len(res.Rejected) > 0 {
		rejection = res.Rejected[0].Error
	}
	if errors.Is(err, rooms.ErrServerNotInRoom) || errors.Is(rejection, rooms.ErrServerNotInRoom) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Room not found")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if rejection != nil {
		util.ResponseRejectedEventJSON(w, r, rejection)
		return
	}

	var resp any = util.EmptyJSON
	if response.afterSend != nil {
		resp, err = response.afterSend(room, &ev)
		if err != nil {
			responseMembershipError(w, r, err)
			return
		}
	}
	if streamed, ok := resp.(streamedResponse); ok {
		streamed.write(w, r)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, resp)
}

func responseMembershipError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, rooms.ErrStateDAGTooLarge) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MTooLarge, err.Error())
		return
	}
	util.ResponseErrorUnknownJSON(w, r, err)
}

// responseIncompatibleRoomVersion refuses a request about a room of a version this server does not
// federate, naming the version as the spec asks
func responseIncompatibleRoomVersion(w http.ResponseWriter, r *http.Request, roomVersion, message string) {
	util.ResponseJSON(w, r, http.StatusBadRequest, map[string]string{
		"errcode":      mautrix.MIncompatibleRoomVersion.ErrCode,
		"error":        message,
		"room_version": roomVersion,
	})
}
