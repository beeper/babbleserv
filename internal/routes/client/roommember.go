// Room member endpoints
//
// Note that these are mostly quite simple - most endpoints require the requesting
// user being in the room already, we can just offload that work to the send events
// transaction which will authorize them against the current state.
//
// This also has the neat side effect of ensuring that this server is present
// in the room as well, since we track server membership changes within the send
// event transaction.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type reqMemberSelf struct {
	Reason string `json:"reason,omitempty"`
}

type reqMemberOther struct {
	reqMemberSelf `json:",inline"`
	UserID        id.UserID `json:"user_id"`
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3joined_rooms
func (c *ClientRoutes) GetJoinedRooms(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	memberships, err := c.db.Rooms.GetUserMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	roomIDs := make([]id.RoomID, 0)
	for _, membership := range memberships {
		if membership.Membership == event.MembershipJoin {
			roomIDs = append(roomIDs, membership.RoomID)
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespJoinedRooms{JoinedRooms: roomIDs})
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidforget
func (c *ClientRoutes) ForgetRoom(w http.ResponseWriter, r *http.Request) {
	// Check membership = left
	// If so, drop users membership, don't modify room state at all
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidinvite
func (c *ClientRoutes) SendRoomInvite(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	req, respErr := util.ParseRequestJSON[reqMemberOther](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	otherUserID := req.UserID
	_, homeserver, err := otherUserID.Parse()
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid user ID")
		return
	}

	sKey := otherUserID.String()
	userID := middleware.GetRequestUserID(r)
	content := makeMembershipContent(event.MembershipInvite, req.Reason)
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)

	inviteStateEvs, err := c.db.Rooms.GetCurrentRoomStrippedStateEvents(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	partialEv.SetUnsigned("invite_room_state", inviteStateEvs)

	if homeserver == c.config.ServerName {
		_, err := c.db.Accounts.GetLocalUser(r.Context(), otherUserID)
		if errors.Is(err, types.ErrUserNotFound) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Unknown user")
			return
		} else if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		c.sendLocalEventHandleResults(w, r, roomID, partialEv, func(ev *types.Event) any {
			return util.EmptyJSON
		})
	} else {
		// We're inviting a remote user, we need the users HS to sign the event before we send it.
		// https://spec.matrix.org/v1.11/server-server-api/#inviting-to-a-room
		_, respErr, err := c.prepareAndSendInviteForRemoteUser(r.Context(), roomID, otherUserID, partialEv)
		if err != nil {
			if respErr != nil {
				util.ResponseErrorMessageJSON(w, r, *respErr, err.Error())
				return
			}
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3joinroomidoralias
func (c *ClientRoutes) SendRoomJoinAlias(w http.ResponseWriter, r *http.Request) {
	c.sendRoomJoin(w, r)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidjoin
func (c *ClientRoutes) SendRoomJoin(w http.ResponseWriter, r *http.Request) {
	c.sendRoomJoin(w, r)
}

// Joon a room by alias or ID
func (c *ClientRoutes) sendRoomJoin(w http.ResponseWriter, r *http.Request) {
	roomID := c.getRoomIDFromRequest(r, "roomID")
	if roomID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var req reqMemberSelf
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	serverInRoom, err := c.db.Rooms.IsServerJoinedRoom(r.Context(), c.config.ServerName, roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	userID := middleware.GetRequestUserID(r)

	if serverInRoom {
		// The easy path - we're already in the room, so just send the join. We
		// re-check the server in room within the send local transaction.
		sKey := userID.String()
		content := makeMembershipContent(event.MembershipJoin, req.Reason)
		ev := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)
		c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
			return util.EmptyJSON
		})
	} else {
		// TODO: lookup any invite
		otherServers := getOtherServers(r, roomID)
		ev, otherServer, err := c.makeFederatedEvent(r, roomID, otherServers, func(otherServer string) (federatedMakeResp, error) {
			makeJoinResp, err := c.fclient.MakeJoin(
				r.Context(),
				spec.ServerName(c.config.ServerName),
				spec.ServerName(otherServer),
				roomID.String(),
				userID.String(),
			)
			return federatedMakeResp{
				makeJoinResp.JoinEvent,
				makeJoinResp.RoomVersion,
			}, err
		})
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		// Switch to a background context here - if the client drops the request
		// we should still send/receive the join so the state on the remote HS
		// and local don't end up diverged.
		backgroundCtx := hlog.FromRequest(r).With().
			Str("background_task", "SendFederatedJoin").
			Str("server", otherServer).
			Logger().
			WithContext(context.Background())

		sendJoinResp, err := c.fclient.SendJoin(
			backgroundCtx,
			spec.ServerName(c.config.ServerName),
			spec.ServerName(otherServer),
			ev.PDU(),
		)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		// Merge the state + auth events, verifying each
		eventCount := len(sendJoinResp.StateEvents) + len(sendJoinResp.AuthEvents)
		allEvs := make([]*types.Event, 0, eventCount)
		seenIDs := make(map[id.EventID]struct{}, eventCount)
		for _, b := range append(sendJoinResp.StateEvents, sendJoinResp.AuthEvents...) {
			remoteEv := &types.Event{RoomVersion: ev.RoomVersion}
			if err := json.Unmarshal(b, remoteEv); err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
			// Verify the event is signed by the senders server (which may not be the one we are
			// joining the room via).
			verifyErr, err := util.VerifyEvent(backgroundCtx, remoteEv, remoteEv.Sender.Homeserver(), c.keyStore)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			} else if verifyErr != nil {
				zerolog.Ctx(backgroundCtx).
					Err(verifyErr).
					Stringer("event_id", remoteEv.ID).
					Stringer("type", remoteEv.Type).
					Any("ev", remoteEv).
					Msg("Skipping event that failed verification during join")
				continue
			}
			if _, found := seenIDs[remoteEv.ID]; found {
				zerolog.Ctx(backgroundCtx).Warn().
					Stringer("event_id", remoteEv.ID).
					Msg("Skipping duplicate event in join response")
				continue
			}
			allEvs = append(allEvs, remoteEv)
			seenIDs[remoteEv.ID] = struct{}{}
		}
		// Finally, add our join event we just sent to the server
		allEvs = append(allEvs, ev)

		// TODO: check for and fill any missing auth events here
		// THIS SHOULD NEVER HAPPEN? Synapse on beeper-dev misses an event in the auth chain

		util.SortEventList(allEvs)

		if _, err = c.db.Rooms.SendFederatedEvents(
			backgroundCtx, roomID, allEvs,
			rooms.SendFederatedEventsOptions{
				// We're joining *now* and won't have all prev event history, ultimately we have
				// to trust the other HS is giving us the correct state.
				RemoteJoinEventID: ev.ID,
			},
		); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		util.ResponseJSON(w, r, http.StatusOK, struct {
			RoomID id.RoomID `json:"room_id"`
		}{roomID})
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3knockroomidoralias
func (c *ClientRoutes) SendRoomKnockAlias(w http.ResponseWriter, r *http.Request) {
	roomID := c.getRoomIDFromRequest(r, "roomID")
	if roomID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var req reqMemberSelf
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	serverInRoom, err := c.db.Rooms.IsServerJoinedRoom(r.Context(), c.config.ServerName, roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	userID := middleware.GetRequestUserID(r)

	if serverInRoom {
		// The easy path - we're (the server) already in the room, so send the knock locally to be
		// send out over federation.
		sKey := userID.String()
		content := makeMembershipContent(event.MembershipKnock, req.Reason)
		ev := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)
		c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
			return util.EmptyJSON
		})
	} else {
		otherServers := getOtherServers(r, roomID)
		ev, otherServer, err := c.makeFederatedEvent(r, roomID, otherServers, func(otherServer string) (federatedMakeResp, error) {
			makeJoinResp, err := c.fclient.MakeKnock(
				r.Context(),
				spec.ServerName(c.config.ServerName),
				spec.ServerName(otherServer),
				roomID.String(),
				userID.String(),
				[]gomatrixserverlib.RoomVersion{},
			)
			return federatedMakeResp{
				makeJoinResp.KnockEvent,
				makeJoinResp.RoomVersion,
			}, err
		})
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		// Switch to a background context here - if the client drops the request we should still
		// send/receive the knock so the state on the remote HS and local don't end up diverged.
		backgroundCtx := hlog.FromRequest(r).With().
			Str("background_task", "SendFederatedKnock").
			Logger().
			WithContext(context.Background())

		sendKnockResp, err := c.fclient.SendKnock(
			backgroundCtx,
			spec.ServerName(c.config.ServerName),
			spec.ServerName(otherServer),
			ev.PDU(),
		)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		ev.SetUnsigned("knock_room_state", sendKnockResp.KnockRoomState)

		if err := c.db.Rooms.SendFederatedOutlierMembershipEvent(r.Context(), ev); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidleave
func (c *ClientRoutes) SendRoomLeave(w http.ResponseWriter, r *http.Request) {
	var req reqMemberSelf
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}
	userID := middleware.GetRequestUserID(r)
	c.sendRoomLeaveOrKick(w, r, userID, req.Reason)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidkick
func (c *ClientRoutes) SendRoomKick(w http.ResponseWriter, r *http.Request) {
	var req reqMemberOther
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}
	c.sendRoomLeaveOrKick(w, r, req.UserID, req.Reason)
}

// Leave/kick are effectively the same thing with different target users (self or other)
func (c *ClientRoutes) sendRoomLeaveOrKick(w http.ResponseWriter, r *http.Request, leavingUserID id.UserID, reason string) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	isLeave := middleware.GetRequestUserID(r) == leavingUserID

	// This is the CS API, we're the sender
	sendingServerInRoom, err := c.db.Rooms.IsServerJoinedRoom(r.Context(), c.config.ServerName, roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// Find membership of the leaving user
	var membership event.Membership
	mtup, err := c.db.Rooms.GetUserMembership(r.Context(), leavingUserID, roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if mtup != nil {
		membership = mtup.Membership
	}
	switch membership {
	case event.MembershipJoin, event.MembershipInvite, event.MembershipKnock:
		// Valid - we can reject invites and retract knocks
	default:
		// Invalid, we're not joined (server not in room) so we're already left, ban or empty
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, fmt.Sprintf("Current membership is not join, invite or knock: %s", membership))
		return
	}

	// Find the "other" server (based on the invite/join/knock we're replacing), there's two cases
	// - we're rejecting an invite - we want the HS from the invites sender
	// - we're rescinding an invite - we want the HS from the invites state key
	currentMemberEvent, err := c.db.Rooms.GetEvent(r.Context(), mtup.EventID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if currentMemberEvent == nil {
		panic("got membership tup with missing event!")
	}
	var otherHomeserver string
	if isLeave {
		// We're the leaving user, so looking for invite senders
		otherHomeserver = currentMemberEvent.Sender.Homeserver()
	} else {
		// We're kicking someone else, so look for membership targets
		otherHomeserver = id.UserID(*currentMemberEvent.StateKey).Homeserver()
	}
	// Assuming not us, check if the other HS is joined to the room
	if otherHomeserver == c.config.ServerName {
		otherHomeserver = ""
	}

	sendingUserID := middleware.GetRequestUserID(r)
	sKey := leavingUserID.String()
	content := makeMembershipContent(event.MembershipLeave, reason)
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, sendingUserID, content)

	// We're in the room - send the event locally and then, if the other HS isn't in the room, send
	// it over to them.
	if sendingServerInRoom {
		c.sendLocalEventHandleResults(w, r, roomID, partialEv, func(ev *types.Event) any {
			if otherHomeserver != "" {
				if err := c.fclient.SendLeave(
					r.Context(),
					spec.ServerName(c.config.ServerName),
					spec.ServerName(otherHomeserver),
					ev.PDU(),
				); err != nil {
					// Log, but don't error - the local user should still get their leave
					hlog.FromRequest(r).Err(err).Msg("Failed to send federated leave to nonjoined server")
				}
			}
			return util.EmptyJSON
		})
		return
	}

	// We're not in the room but do know the other HS, so use them via the make_leave, send_leave
	if otherHomeserver != "" {
		leaveEv, otherServer, err := c.makeFederatedEvent(r, roomID, []string{otherHomeserver}, func(otherServer string) (federatedMakeResp, error) {
			makeJoinResp, err := c.fclient.MakeLeave(
				r.Context(),
				spec.ServerName(c.config.ServerName),
				spec.ServerName(otherServer),
				roomID.String(),
				leavingUserID.String(),
			)
			return federatedMakeResp{
				makeJoinResp.LeaveEvent,
				makeJoinResp.RoomVersion,
			}, err
		})
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if err := c.fclient.SendLeave(
			r.Context(),
			spec.ServerName(c.config.ServerName),
			spec.ServerName(otherServer),
			leaveEv.PDU(),
		); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if err := c.db.Rooms.SendFederatedOutlierMembershipEvent(r.Context(), leaveEv); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
		return
	}

	// We're not in the room and we've no idea who, if any, the other HS is supposed to be, so we
	// just throw together a half complete local event and send as an outlier. This means our local
	// user can always leave themselves from rooms. We reject kicks here.
	if !isLeave {
		util.ResponseErrorUnknownJSON(w, r, fmt.Errorf("cannot kick unknown nonlocal user from unknown room"))
		return
	}

	var roomVersion string
	room, err := c.db.Rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if room != nil {
		roomVersion = room.Version
	} else {
		roomVersion = c.config.Rooms.DefaultVersion // TODO: what do we do here?
	}
	outlierLeaveEv := &types.Event{
		RoomVersion:  roomVersion,
		PartialEvent: *partialEv,
	}

	keyID, key := c.config.MustGetActiveSigningKey()
	util.HashAndSignEvent(outlierLeaveEv, c.config.ServerName, keyID, key)

	// Now send it as a local outlier
	if err := c.db.Rooms.SendFederatedOutlierMembershipEvent(r.Context(), outlierLeaveEv); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidban
func (c *ClientRoutes) SendRoomBan(w http.ResponseWriter, r *http.Request) {
	c.sendBanOrUnban(w, r, event.MembershipBan)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidunban
func (c *ClientRoutes) SendRoomUnban(w http.ResponseWriter, r *http.Request) {
	c.sendBanOrUnban(w, r, event.MembershipLeave)
}

// Ban/unban behave the same - local sends (requesting user, thus this server, must be in room)
func (c *ClientRoutes) sendBanOrUnban(w http.ResponseWriter, r *http.Request, membership event.Membership) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	var req reqMemberOther
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	sKey := req.UserID.String()
	userID := middleware.GetRequestUserID(r)
	content := makeMembershipContent(membership, req.Reason)
	ev := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)

	c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
		return util.EmptyJSON
	})
}
