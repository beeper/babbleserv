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
	"slices"
	"time"

	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/transient"
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
	roomIDs, err := c.db.Rooms.JoinedRooms(r.Context(), middleware.GetRequestUserID(r))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
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

	req, respErr := util.ParseOptionalRequestJSON[reqMemberOther](r)
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

	inviteStateEvs, err := c.db.Rooms.RoomStrippedState(r.Context(), roomID)
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

	req, respErr := util.ParseOptionalRequestJSON[reqMemberSelf](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	serverInRoom, err := c.db.Rooms.IsServerJoined(r.Context(), roomID, c.config.ServerName)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	userID := middleware.GetRequestUserID(r)

	if serverInRoom {
		// The easy path - we're already in the room, so just send the join. The send local
		// transaction fails with ErrServerNotInRoom should this server have left meanwhile.
		sKey := userID.String()
		content := makeMembershipContent(event.MembershipJoin, req.Reason)
		ev := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)
		c.sendLocalEventHandleResults(w, r, roomID, ev, func(ev *types.Event) any {
			return util.EmptyJSON
		})
	} else if err := c.remoteJoin(r, roomID, userID, getOtherServers(r, roomID)); err != nil {
		responseRemoteMembershipError(w, r, err)
	} else {
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

	req, respErr := util.ParseRequestJSON[reqMemberSelf](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	serverInRoom, err := c.db.Rooms.IsServerJoined(r.Context(), roomID, c.config.ServerName)
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
	} else if err := c.remoteMembership(r, roomID, event.MembershipKnock, userID, getOtherServers(r, roomID)); err != nil {
		responseRemoteMembershipError(w, r, err)
	} else {
		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidleave
func (c *ClientRoutes) SendRoomLeave(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseOptionalRequestJSON[reqMemberSelf](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	userID := middleware.GetRequestUserID(r)
	c.sendRoomLeaveOrKick(w, r, userID, req.Reason)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidkick
func (c *ClientRoutes) SendRoomKick(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseOptionalRequestJSON[reqMemberOther](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	c.sendRoomLeaveOrKick(w, r, req.UserID, req.Reason)
}

// Leave/kick are effectively the same thing with different target users (self or other)
func (c *ClientRoutes) sendRoomLeaveOrKick(w http.ResponseWriter, r *http.Request, leavingUserID id.UserID, reason string) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	isLeave := middleware.GetRequestUserID(r) == leavingUserID

	// This is the CS API, we're the sender
	serverInRoom, err := c.db.Rooms.IsServerJoined(r.Context(), roomID, c.config.ServerName)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// Find membership of the leaving user
	var membership event.Membership
	mtup, currentMemberEv, err := c.db.Rooms.GetCurrentMembershipAndEvent(r.Context(), leavingUserID, roomID)
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
	var otherHomeserver string
	if isLeave {
		// We're the leaving user, so looking for invite senders HS
		otherHomeserver = currentMemberEv.Sender.Homeserver()
	} else {
		// We're kicking someone else, so look for membership targets
		otherHomeserver = id.UserID(*currentMemberEv.StateKey).Homeserver()
	}
	if otherHomeserver == c.config.ServerName {
		otherHomeserver = ""
	}

	sendingUserID := middleware.GetRequestUserID(r)
	sKey := leavingUserID.String()
	content := makeMembershipContent(event.MembershipLeave, reason)
	partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, sendingUserID, content)

	// We're in the room - send the event locally and then, if it rescinds an invite or knock whose
	// target HS isn't in the room, send it over to them. A rejection is not sent to an inviting HS
	// outside the room, which has no membership to update.
	if serverInRoom {
		var outlierServer string
		if !isLeave && membership != event.MembershipJoin && otherHomeserver != "" {
			otherServerInRoom, err := c.db.Rooms.IsServerJoined(r.Context(), roomID, otherHomeserver)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			} else if !otherServerInRoom {
				outlierServer = otherHomeserver
			}
		}
		c.sendLocalEventHandleResults(w, r, roomID, partialEv, func(ev *types.Event) any {
			if outlierServer == "" {
				return util.EmptyJSON
			}

			td := &types.ToDevice{
				UserID:  id.UserID("@:" + outlierServer),
				Type:    types.BabbleservRemoteOutlierEvent,
				Content: exerrors.Must(json.Marshal(ev)),
			}

			_, err := c.db.SendToDeviceEvents(r.Context(), []*types.ToDevice{td}, transient.SendToDeviceOptions{})
			if err != nil {
				hlog.FromRequest(r).Err(err).
					Msg("Failed to send federated leave event over to-device to nonjoined server")
			}
			return util.EmptyJSON
		})
		return
	}

	// Try rejecting through the other HS. An invite can still be rejected locally if that server
	// is unavailable or has left the room. Local storage errors must still fail the request.
	if otherHomeserver != "" {
		if err := c.remoteMembership(r, roomID, event.MembershipLeave, leavingUserID, []string{otherHomeserver}); err != nil {
			if !isLeave || membership != event.MembershipInvite || !errors.Is(err, errRemoteLeaveFailed) {
				responseRemoteMembershipError(w, r, err)
				return
			}
			hlog.FromRequest(r).Warn().Err(err).
				Stringer("room_id", roomID).
				Str("server", otherHomeserver).
				Msg("Failed to reject invite over federation, recording leave locally")
		} else {
			util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
			return
		}
	}

	// With no other HS, or after a failed remote invite rejection, record the local user's leave
	// as an outlier. This changes their membership stream without publishing room state.
	if !isLeave {
		util.ResponseErrorUnknownJSON(w, r, fmt.Errorf("cannot kick unknown nonlocal user from unknown room"))
		return
	}

	outlierLeaveEv := &types.Event{
		RoomVersion:  currentMemberEv.RoomVersion,
		PartialEvent: *partialEv,
		Local:        true,
		PrevEventIDs: []id.EventID{currentMemberEv.ID},
	}
	outlierLeaveEv.Timestamp = time.Now().UTC().UnixMilli()

	keyID, key := c.config.MustGetActiveSigningKey()
	if err := util.HashAndSignEvent(outlierLeaveEv, c.config.ServerName, keyID, key); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

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

	req, respErr := util.ParseOptionalRequestJSON[reqMemberOther](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
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

// A make_leave or send_leave failure, before the membership is stored locally
var errRemoteLeaveFailed = errors.New("remote leave failed")

// remoteJoin joins a local user to a room this server is not in. The join made through make_join
// becomes a target of the room's ingest job, which sends it and publishes it, while the request waits
// for the job to publish or fail it. A join of the user the job has pending already, as a retried
// /join finds, is waited on instead of making another.
func (c *ClientRoutes) remoteJoin(r *http.Request, roomID id.RoomID, userID id.UserID, servers []string) error {
	ctx := r.Context()
	log := hlog.FromRequest(r).With().Stringer("room_id", roomID).Logger()
	pending, err := c.db.Rooms.GetPendingIngestJoin(ctx, roomID, userID)
	if err != nil {
		return err
	}
	if pending != "" {
		log.Info().Stringer("event_id", pending).Msg("Waiting on the user's pending join")
	} else {
		origin := spec.ServerName(c.config.ServerName)
		ev, server, err := c.makeFederatedEvent(r, roomID, event.MembershipJoin, servers, func(server string) (federatedMakeResp, error) {
			resp, err := c.fclient.MakeJoin(ctx, origin, spec.ServerName(server), roomID.String(), userID.String())
			return federatedMakeResp{resp.JoinEvent, resp.RoomVersion}, err
		})
		if err != nil {
			return err
		}
		joinServers := slices.Concat([]string{server}, slices.DeleteFunc(slices.Clone(servers), func(other string) bool {
			return other == server || other == c.config.ServerName
		}))
		eventID, err := c.db.Rooms.EnqueueIngestJoin(ctx, ev, joinServers)
		if err != nil {
			return err
		}
		log.Info().Stringer("event_id", eventID).Str("server", server).Msg("Enqueued remote join")
	}
	c.ingestJobs.Run(roomID)
	return c.waitForJoin(log.WithContext(ctx), roomID, userID)
}

// The user's join is read again this often, should its watch fail or miss a change
const joinWaitPoll = 5 * time.Second

var errJoinIncomplete = errors.New("the join finished without the user joined")

// waitForJoin waits for the room's ingest job to publish the user's join, or fail it. The job clears
// the user's iju in the transaction publishing the join, and records a failure there.
func (c *ClientRoutes) waitForJoin(ctx context.Context, roomID id.RoomID, userID id.UserID) error {
	for {
		join, watch, err := c.db.Rooms.WatchIngestJoin(ctx, roomID, userID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to watch the join, polling it instead")
			if join, err = c.db.Rooms.GetIngestJoin(ctx, roomID, userID); err != nil {
				return err
			}
		}
		if (join == nil || join.Failure != nil) && watch != nil {
			watch.Cancel()
		}
		if join == nil {
			if joined, err := c.db.Rooms.IsUserJoinedRoom(ctx, userID, roomID); err != nil {
				return err
			} else if !joined {
				return errJoinIncomplete
			}
			return nil
		} else if join.Failure != nil {
			return join.Failure
		}
		if err := waitForChange(ctx, watch, joinWaitPoll); err != nil {
			return err
		}
	}
}

// waitForChange waits until the watch fires, the poll interval passes or the context ends. A watch
// that fails, as one past FoundationDB's limit on watches does, leaves the poll.
func waitForChange(ctx context.Context, watch fdb.FutureNil, poll time.Duration) error {
	timer := time.NewTimer(poll)
	defer timer.Stop()
	fired := make(chan error, 1)
	if watch != nil {
		defer watch.Cancel()
		go func() { fired <- watch.Get() }()
	}
	for {
		select {
		case err := <-fired:
			if err == nil {
				return nil
			}
			zerolog.Ctx(ctx).Debug().Err(err).Msg("Join watch failed, polling it")
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// remoteMembership sends target's knock or leave of a room this server is not in via make_/send_
// endpoints
func (c *ClientRoutes) remoteMembership(
	r *http.Request,
	roomID id.RoomID,
	membership event.Membership,
	target id.UserID,
	servers []string,
) error {
	origin := spec.ServerName(c.config.ServerName)
	ev, server, err := c.makeFederatedEvent(r, roomID, membership, servers, func(server string) (federatedMakeResp, error) {
		destination := spec.ServerName(server)
		switch membership {
		case event.MembershipKnock:
			resp, err := c.fclient.MakeKnock(r.Context(), origin, destination, roomID.String(), target.String(), c.joinedRoomVersions())
			return federatedMakeResp{resp.KnockEvent, resp.RoomVersion}, err
		default:
			resp, err := c.fclient.MakeLeave(r.Context(), origin, destination, roomID.String(), target.String())
			return federatedMakeResp{resp.LeaveEvent, resp.RoomVersion}, err
		}
	})
	if err != nil {
		if membership == event.MembershipLeave {
			return fmt.Errorf("%w: %w", errRemoteLeaveFailed, err)
		}
		return err
	}

	ctx := hlog.FromRequest(r).With().
		Str("background_task", "SendFederatedMembership").
		Str("membership", string(membership)).
		Str("server", server).
		Logger().
		WithContext(context.Background())
	destination := spec.ServerName(server)
	switch membership {
	case event.MembershipKnock:
		resp, err := c.fclient.SendKnock(ctx, origin, destination, ev.PDU())
		if err != nil {
			return err
		}
		ev.SetUnsigned("knock_room_state", resp.KnockRoomState)
	default:
		if err := c.fclient.SendLeave(ctx, origin, destination, ev.PDU()); err != nil {
			return fmt.Errorf("%w: %w", errRemoteLeaveFailed, err)
		}
	}
	return c.db.Rooms.SendFederatedOutlierMembershipEvent(ctx, ev)
}

func responseRemoteMembershipError(w http.ResponseWriter, r *http.Request, err error) {
	var failure *types.IngestFailure
	if errors.As(err, &failure) {
		hlog.FromRequest(r).Warn().
			Int("status", failure.Status).
			Str("errcode", failure.ErrCode).
			Str("error", failure.Message).
			Msg("Remote join failed")
		util.ResponseJSON(w, r, failure.Status, map[string]string{"errcode": failure.ErrCode, "error": failure.Message})
		return
	}
	responseMakeFederatedEventError(w, r, err)
}
