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

type reqMemberJoin struct {
	reqMemberSelf `json:",inline"`
	// TODO: support third party join rules
	// ThirdPartySigned struct{} `json:"third_party_signed"`
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

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidinvite
func (c *ClientRoutes) SendRoomInvite(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	req, respErr := util.ParseRequestJSON[reqMemberOther](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	otherUserID := req.UserID
	username, homeserver, err := otherUserID.ParseAndValidate()
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid user ID")
		return
	}

	sKey := otherUserID.String()
	userID := middleware.GetRequestUserID(r)
	content := makeMembershipContent(event.MembershipInvite, req.Reason)
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)

	if homeserver == c.config.ServerName {
		_, err := c.db.Accounts.GetLocalUser(r.Context(), username)
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
		// We're inviting a remote user, we need the users HS to sign the event
		// before we send it.
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

	serverInRoom, err := c.db.Rooms.IsServerInRoom(r.Context(), c.config.ServerName, roomID)
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
		ev, otherServer, err := c.makeFederatedEvent(r, roomID, func(otherServer string) (federatedMakeResp, error) {
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
			verifyErr, err := util.VerifyEvent(backgroundCtx, remoteEv, remoteEv.Origin, c.keyStore)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			} else if verifyErr != nil {
				zerolog.Ctx(backgroundCtx).
					Err(verifyErr).
					Str("event_id", remoteEv.ID.String()).
					Any("event", remoteEv).
					Msg("Skipping error that failed verification during join")
				continue
			}
			if _, found := seenIDs[remoteEv.ID]; found {
				zerolog.Ctx(backgroundCtx).Warn().
					Str("event_id", remoteEv.ID.String()).
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
			rooms.SendFederatedEventsOptions{},
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

	serverInRoom, err := c.db.Rooms.IsServerInRoom(r.Context(), c.config.ServerName, roomID)
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
		ev, otherServer, err := c.makeFederatedEvent(r, roomID, func(otherServer string) (federatedMakeResp, error) {
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

		// Switch to a background context here - if the client drops the request
		// we should still send/receive the join so the state on the remote HS
		// and local don't end up diverged.
		backgroundCtx := hlog.FromRequest(r).With().
			Str("background_task", "SendFederatedJoin").
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

		_ = sendKnockResp

		// TODO: send outlier membership knock
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidleave
func (c *ClientRoutes) SendRoomLeave(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")

	var req reqMemberSelf
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	serverInRoom, err := c.db.Rooms.IsServerInRoom(r.Context(), c.config.ServerName, roomID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	if serverInRoom {
		// The easy path - we're (the server) in the room, we can just send the
		// leave. Note that this might also mean the server is no longer in the
		// room once sent.
		userID := middleware.GetRequestUserID(r)
		sKey := userID.String()
		content := makeMembershipContent(event.MembershipLeave, req.Reason)
		ev := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)
		c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
			return util.EmptyJSON
		})
	} else {
		// The complicated path - we're not in the room, so presumably we're
		// rejecting an invite over federation:
		// https://spec.matrix.org/v1.11/server-server-api/#leaving-rooms-rejecting-invites

		// TODO: make_leave, send_leave
		// TODO: persist locally? Yes, SendFederatedOutlierMembershipEvent
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidforget
func (c *ClientRoutes) ForgetRoom(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
	// ???
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidkick
func (c *ClientRoutes) SendRoomKick(w http.ResponseWriter, r *http.Request) {
	c.sendOtherUserMemberEvent(w, r, event.MembershipLeave)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidban
func (c *ClientRoutes) SendRoomBan(w http.ResponseWriter, r *http.Request) {
	c.sendOtherUserMemberEvent(w, r, event.MembershipBan)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidunban
func (c *ClientRoutes) SendRoomUnban(w http.ResponseWriter, r *http.Request) {
	c.sendOtherUserMemberEvent(w, r, event.MembershipLeave)
}

// Kick, ban, unban all behave the same
func (c *ClientRoutes) sendOtherUserMemberEvent(w http.ResponseWriter, r *http.Request, membership event.Membership) {
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
