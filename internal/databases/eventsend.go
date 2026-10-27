package databases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
)

// Wrapper around rooms.SendLocalEvents that pre-fetches local user push rules and room context
func (d *Databases) SendLocalEvents(
	ctx context.Context,
	roomID id.RoomID,
	partialEvs []*types.PartialEvent,
	options rooms.SendLocalEventsOptions,
) (*rooms.SendEventsResult, error) {
	return d.sendEventsFunc(ctx, roomID, func(userPushRules types.UserPushRulesMap, userRoomContext types.UserRoomContextMap) (*rooms.SendEventsResult, error) {
		return d.Rooms.SendLocalEvents(ctx, roomID, partialEvs, userPushRules, userRoomContext, options)
	})
}

// Wrapper around rooms.SendFederatedEvents that pre-fetches local user push rules and room context
func (d *Databases) SendFederatedEvents(
	ctx context.Context,
	roomID id.RoomID,
	evs []*types.Event,
	given *rooms.GivenStates,
	options rooms.SendFederatedEventsOptions,
) (*rooms.SendEventsResult, error) {
	return d.sendEventsFunc(ctx, roomID, func(userPushRules types.UserPushRulesMap, userRoomContext types.UserRoomContextMap) (*rooms.SendEventsResult, error) {
		return d.Rooms.SendFederatedEvents(ctx, roomID, evs, given, userPushRules, userRoomContext, options)
	})
}

// RemoteJoin is a local user's join to a room through another server, see rooms.RemoteJoin, which
// joins with a new local event when another join got this server into the room meanwhile and the
// send_join event cannot become the user's membership
type RemoteJoin struct {
	*rooms.RemoteJoin
	d      *Databases
	joinEv *types.Event
}

// NewRemoteJoin prepares a local user's join to a room through another server, with the state before
// the join and its auth chain from the resident server, and the local members' push rules
func (d *Databases) NewRemoteJoin(
	ctx context.Context,
	joinEv *types.Event,
	stateIDs []id.EventID,
	evs []*types.Event,
) (*RemoteJoin, error) {
	userPushRules, userRoomContext, err := d.pushContext(ctx, joinEv.RoomID)
	if err != nil {
		return nil, err
	}
	join, err := d.Rooms.NewRemoteJoin(ctx, joinEv, stateIDs, evs, userPushRules, userRoomContext)
	if err != nil {
		return nil, err
	}
	return &RemoteJoin{RemoteJoin: join, d: d, joinEv: joinEv}, nil
}

// NewStateDAGRemoteJoin prepares a local user's join to a state DAG room through another server,
// once the room's ingest job replayed the state DAG the resident served, whose tips are given, see
// rooms.RoomsDatabase.NewStateDAGRemoteJoin
func (d *Databases) NewStateDAGRemoteJoin(ctx context.Context, joinEv *types.Event, tips []id.EventID) (*RemoteJoin, error) {
	userPushRules, userRoomContext, err := d.pushContext(ctx, joinEv.RoomID)
	if err != nil {
		return nil, err
	}
	join, err := d.Rooms.NewStateDAGRemoteJoin(ctx, joinEv, tips, userPushRules, userRoomContext)
	if err != nil {
		return nil, err
	}
	return &RemoteJoin{RemoteJoin: join, d: d, joinEv: joinEv}, nil
}

// Publish publishes the join. While this server is not in the room the join takes the response
// state as the room's state, otherwise it is sent like any federated event, against that state and
// the room's current state.
func (j *RemoteJoin) Publish(ctx context.Context) (*rooms.SendEventsResult, error) {
	res, err := j.RemoteJoin.Publish(ctx)
	if err != nil {
		return nil, err
	}
	dropped := len(res.Rejected) > 0 && errors.Is(res.Rejected[0].Error, rooms.ErrEventDropped)
	softFailed := slices.ContainsFunc(res.Allowed, func(ev *types.Event) bool { return ev.SoftFailed }) ||
		(len(res.Rejected) > 0 && res.Rejected[0].Event.SoftFailed)
	if dropped || softFailed {
		// Another join got this server into the room meanwhile. A dropped join is not stored and a
		// soft failed one never becomes the user's membership, so the user joins with a new local
		// event instead. Otherwise the send_join event is kept, as the other server has it and may
		// have federated it already.
		var content map[string]any
		if err := json.Unmarshal(j.joinEv.Content, &content); err != nil {
			return nil, fmt.Errorf("failed to parse join content: %w", err)
		}
		delete(content, "join_authorised_via_users_server")
		// Keyed by the send_join event, so a job resumed after this join was sent sends no other
		return j.d.SendLocalEvents(ctx, j.joinEv.RoomID, []*types.PartialEvent{
			types.NewPartialEvent(j.joinEv.RoomID, event.StateMember, j.joinEv.StateKey, j.joinEv.Sender, content),
		}, rooms.SendLocalEventsOptions{
			TransactionDevice:   &types.UserDevice{UserID: j.joinEv.Sender, DeviceID: remoteJoinDeviceID},
			TransactionEndpoint: remoteJoinEndpoint,
			TransactionID:       j.joinEv.ID.String(),
		})
	}
	return res, nil
}

// Scope the transaction ID of a remote join's local fallback, which no client device sees
const (
	remoteJoinDeviceID = id.DeviceID("BABBLESERV_REMOTE_JOIN")
	remoteJoinEndpoint = "remote-join"
)

func (d *Databases) sendEventsFunc(
	ctx context.Context,
	roomID id.RoomID,
	fn func(types.UserPushRulesMap, types.UserRoomContextMap) (*rooms.SendEventsResult, error),
) (*rooms.SendEventsResult, error) {
	userPushRules, userRoomContext, err := d.pushContext(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return fn(userPushRules, userRoomContext)
}

// pushContext returns the push rules and room context of the room's local joined members
func (d *Databases) pushContext(ctx context.Context, roomID id.RoomID) (types.UserPushRulesMap, types.UserRoomContextMap, error) {
	// Get room for member count - this means the member count for push rules does *not* consider
	// any member events in the batch being persisted.
	room, err := d.Rooms.GetRoom(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}

	var memberCount int
	if room != nil {
		memberCount = room.MemberCount
	}

	// Get local users in room
	memberships, err := d.Rooms.LocalJoinedMembers(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}

	// Build push rules map for each local user
	userPushRules := make(types.UserPushRulesMap, len(memberships))
	userRoomContext := make(types.UserRoomContextMap, len(memberships))

	for userID := range memberships {
		// TODO: GetRulesForUsers in parallel
		ruleset, err := d.Accounts.GetPushRulesForUser(ctx, userID)
		if err != nil {
			return nil, nil, err
		}

		context := &types.PushRuleRoom{
			MemberCount:    memberCount,
			OwnDisplayname: userID.String(),
		}

		// TODO: GetProfilesForUsers in parallel
		if profile, err := d.Accounts.GetUserProfile(ctx, userID); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to get user profile, falling back to userID")
		} else if profile != nil && profile.DisplayName != "" {
			context.OwnDisplayname = profile.DisplayName
		}

		userPushRules[userID] = ruleset
		userRoomContext[userID] = context
	}

	return userPushRules, userRoomContext, nil
}
