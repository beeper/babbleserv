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
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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

func makeMembershipContent(membership event.Membership, reason string) map[string]any {
	content := map[string]any{
		"membership": membership,
	}
	if reason != "" {
		content["reason"] = reason
	}
	return content
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3joined_rooms
func (c *ClientRoutes) GetJoinedRooms(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	memberships, err := c.db.Rooms.GetUserMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	var roomIDs []id.RoomID
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

	var req reqMemberOther
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUserID(r)
	otherUserID := req.UserID
	sKey := otherUserID.String()
	content := makeMembershipContent(event.MembershipInvite, req.Reason)
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)

	if otherUserID.Homeserver() == c.config.ServerName {
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
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidjoin
func (c *ClientRoutes) SendRoomJoin(w http.ResponseWriter, r *http.Request) {
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
		// TODO: don't blindly use roomID server here - but use what? Alias?
		// TODO: use invite sender!
		roomIDBits := strings.Split(roomID.String(), ":")
		otherServer := roomIDBits[len(roomIDBits)-1]

		// We're not in the room - we need to do the join dance to get the room
		// current state from one of the remote servers.
		makeJoinResp, err := c.fclient.MakeJoin(
			r.Context(),
			spec.ServerName(c.config.ServerName),
			spec.ServerName(otherServer),
			roomID.String(),
			userID.String(),
		)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		roomVersion := string(makeJoinResp.RoomVersion)

		ev := types.EventFromProtoEvent(makeJoinResp.JoinEvent)
		ev.Timestamp = time.Now().UTC().UnixMilli()
		ev.Origin = c.config.ServerName
		ev.RoomVersion = roomVersion

		keyID, key := c.config.MustGetActiveSigningKey()
		util.HashAndSignEvent(ev, c.config.ServerName, keyID, key)

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
			remoteEv := &types.Event{RoomVersion: roomVersion}
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

		results, err := c.db.Rooms.SendFederatedEvents(
			backgroundCtx, roomID, allEvs,
			rooms.SendFederatedEventsOptions{
				// Skip the prev state check as we are including the full state in our ev list (which
				// will itself be checked). This allows for events to be included that we don't have
				// the prev events for - ie to bootstrap the start of a room from our perspective.
				SkipPrevStateCheck: true,
			},
		)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		hlog.FromRequest(r).Info().Any("SEND JOIN RESP", results).Msg("YIS")

		util.ResponseJSON(w, r, http.StatusOK, struct {
			RoomID id.RoomID `json:"room_id"`
		}{roomID})
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3knockroomidoralias
func (c *ClientRoutes) SendRoomKnockAlias(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
	// Is local server in room?
	// If use: is the requesting user in the room? (we must not)
	// If yes: send knock
	// If no: do the knock dance!
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
