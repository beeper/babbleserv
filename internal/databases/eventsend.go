package databases

import (
	"context"

	"maunium.net/go/mautrix/id"

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
	options rooms.SendFederatedEventsOptions,
) (*rooms.SendEventsResult, error) {
	return d.sendEventsFunc(ctx, roomID, func(userPushRules types.UserPushRulesMap, userRoomContext types.UserRoomContextMap) (*rooms.SendEventsResult, error) {
		return d.Rooms.SendFederatedEvents(ctx, roomID, evs, userPushRules, userRoomContext, options)
	})
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
	memberships, err := d.Rooms.GetCurrentRoomLocalJoinedMemberships(ctx, roomID)
	if err != nil {
		return nil, err
	}

	// Build push rules map for each local user
	userPushRules := make(types.UserPushRulesMap, len(memberships))
	userRoomContext := make(types.UserRoomContextMap, len(memberships))

	for userID := range memberships {
		// TODO: GetRulesForUsers
		ruleset, err := d.Accounts.GetPushRulesForUser(ctx, userID)
		if err != nil {
			// Log but continue - we'll use fallback notification behavior for this user
			continue
		}
		userPushRules[userID] = ruleset
		userRoomContext[userID] = &types.PushRuleRoom{
			MemberCount: memberCount,
			// TODO: use GetSpecificMemberEventsForRoom
			OwnDisplayname: "", // Can be enhanced later to fetch user's displayname
		}
	}

	return fn(userPushRules, userRoomContext)
}
