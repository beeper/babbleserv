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
) (*rooms.SendEventsResult, error) {
	return d.sendEventsFunc(ctx, roomID, func(userPushRules types.UserPushRulesMap, userRoomContext types.UserRoomContextMap) (*rooms.SendEventsResult, error) {
		return d.Rooms.SendFederatedEvents(ctx, roomID, evs, given, userPushRules, userRoomContext)
	})
}

// Sends a local user's join to a room through another server, with the state before the join and
// its auth chain from the resident server's send_join response. While this server is not in the
// room the join takes the response state as the room's state, otherwise it is sent like any
// federated event, against that state and the room's current state.
func (d *Databases) SendRemoteJoin(
	ctx context.Context,
	roomID id.RoomID,
	joinEv *types.Event,
	stateEvs, authEvs []*types.Event,
) (*rooms.SendEventsResult, error) {
	stateIDs := make([]id.EventID, len(stateEvs))
	for i, ev := range stateEvs {
		stateIDs[i] = ev.ID
	}
	res, err := d.SendFederatedEvents(ctx, roomID, []*types.Event{joinEv}, &rooms.GivenStates{
		BeforeEvents: map[id.EventID][]id.EventID{joinEv.ID: stateIDs},
		Events:       slices.Concat(stateEvs, authEvs),
	})
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
		if err := json.Unmarshal(joinEv.Content, &content); err != nil {
			return nil, fmt.Errorf("failed to parse join content: %w", err)
		}
		delete(content, "join_authorised_via_users_server")
		return d.SendLocalEvents(ctx, roomID, []*types.PartialEvent{
			types.NewPartialEvent(roomID, event.StateMember, joinEv.StateKey, joinEv.Sender, content),
		}, rooms.SendLocalEventsOptions{})
	}
	return res, nil
}

func (d *Databases) sendEventsFunc(
	ctx context.Context,
	roomID id.RoomID,
	fn func(types.UserPushRulesMap, types.UserRoomContextMap) (*rooms.SendEventsResult, error),
) (*rooms.SendEventsResult, error) {
	// Get room for member count - this means the member count for push rules does *not* consider
	// any member events in the batch being persisted.
	room, err := d.Rooms.GetRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}

	var memberCount int
	if room != nil {
		memberCount = room.MemberCount
	}

	// Get local users in room
	memberships, err := d.Rooms.LocalJoinedMembers(ctx, roomID)
	if err != nil {
		return nil, err
	}

	// Build push rules map for each local user
	userPushRules := make(types.UserPushRulesMap, len(memberships))
	userRoomContext := make(types.UserRoomContextMap, len(memberships))

	for userID := range memberships {
		// TODO: GetRulesForUsers in parallel
		ruleset, err := d.Accounts.GetPushRulesForUser(ctx, userID)
		if err != nil {
			return nil, err
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

	return fn(userPushRules, userRoomContext)
}
