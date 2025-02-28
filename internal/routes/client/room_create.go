package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const defaultRoomVersion = "11"

var presets = map[string][]struct {
	Type    event.Type
	Content map[string]any
}{
	"private_chat": {
		{event.StateJoinRules, map[string]any{"join_rule": event.JoinRuleInvite}},
		{event.StateHistoryVisibility, map[string]any{"history_visibility": event.HistoryVisibilityShared}},
		{event.StateGuestAccess, map[string]any{"guest_access": event.GuestAccessCanJoin}},
	},
	"trusted_private_chat": {
		{event.StateJoinRules, map[string]any{"join_rule": event.JoinRuleInvite}},
		{event.StateHistoryVisibility, map[string]any{"history_visibility": event.HistoryVisibilityShared}},
		{event.StateGuestAccess, map[string]any{"guest_access": event.GuestAccessCanJoin}},
	},
	"public_chat": {
		{event.StateJoinRules, map[string]any{"join_rule": event.JoinRulePublic}},
		{event.StateHistoryVisibility, map[string]any{"history_visibility": event.HistoryVisibilityShared}},
		{event.StateGuestAccess, map[string]any{"guest_access": event.GuestAccessForbidden}},
	},
}

// https://spec.matrix.org/v1.10/client-server-api/#post_matrixclientv3createroom
func (c *ClientRoutes) CreateRoom(w http.ResponseWriter, r *http.Request) {
	var req mautrix.ReqCreateRoom
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	userID := middleware.GetRequestUserID(r)
	roomID := c.db.Rooms.GenerateRoomID()

	evs := make([]*types.PartialEvent, 0, len(req.InitialState)+5)
	sKey := "" // blank state key to point at

	// Create events per the spec:
	// https://spec.matrix.org/v1.10/client-server-api/#post_matrixclientv3createroom

	// 1: The m.room.create event itself. Must be the first event in the room.
	createContent := make(map[string]any, len(req.CreationContent)+2)
	for key, value := range req.CreationContent {
		createContent[key] = value
	}
	createContent["creator"] = userID
	if req.RoomVersion == "" {
		createContent["room_version"] = c.config.Rooms.DefaultVersion
	} else {
		createContent["room_version"] = req.RoomVersion
	}
	createEv := types.NewPartialEvent(roomID, event.StateCreate, &sKey, userID, createContent)
	evs = append(evs, createEv)

	// 2: An m.room.member event for the creator to join the room. This is needed so the remaining events can be sent.
	userIDStr := string(userID)
	creatorEv := types.NewPartialEvent(roomID, event.StateMember, &userIDStr, userID, map[string]any{
		"membership": event.MembershipJoin,
	})
	evs = append(evs, creatorEv)

	// 3: A default m.room.power_levels event, giving the room creator (and not other members) permission to send state events. Overridden by the power_level_content_override parameter.
	userPowerLevels := map[id.UserID]int{
		userID: 100,
	}
	if req.Preset == "trusted_private_chat" {
		// All invitees are given the same power level as the room creator.
		for _, uid := range req.Invite {
			userPowerLevels[uid] = 100
		}
	}
	powerEv := types.NewPartialEvent(roomID, event.StatePowerLevels, &sKey, userID, map[string]any{
		"users": userPowerLevels,
		"events": map[event.Type]int{
			event.StateHistoryVisibility: 100,
			event.StatePowerLevels:       100,
			event.StateTombstone:         100,
			event.StateServerACL:         100,
		},
	})
	evs = append(evs, powerEv)

	// 4: An m.room.canonical_alias event if room_alias_name is given.
	// TODO: aliases

	// 5: Events set by the preset. Currently these are the m.room.join_rules, m.room.history_visibility, and m.room.guest_access state events.
	preset, found := presets[req.Preset]
	if req.Preset != "" && !found {
		hlog.FromRequest(r).Warn().Msgf("Invalid create room preset: %s", req.Preset)
		return
	}
	for _, ev := range preset {
		evs = append(evs, types.NewPartialEvent(roomID, ev.Type, &sKey, userID, ev.Content))
	}

	// 6: Events listed in initial_state, in the order that they are listed.
	for _, ev := range req.InitialState {
		if ev.ID != "" || ev.RoomID != "" || ev.Sender != "" {
			util.ResponseErrorMessageJSON(
				w, r, mautrix.MInvalidParam,
				"Initial state events should not contain ID, room ID or sender",
			)
			return
		}
		evs = append(evs, types.NewPartialEvent(roomID, ev.Type, ev.StateKey, userID, ev.Content.Raw))
	}

	// 7: Events implied by name and topic (m.room.name and m.room.topic state events).
	if req.Name != "" {
		nameEv := types.NewPartialEvent(roomID, event.StateRoomName, &sKey, userID, map[string]any{"name": req.Name})
		evs = append(evs, nameEv)
	}
	if req.Topic != "" {
		topicEv := types.NewPartialEvent(roomID, event.StateTopic, &sKey, userID, map[string]any{"topic": req.Topic})
		evs = append(evs, topicEv)
	}

	// 8: Invite events implied by invite and invite_3pid (m.room.member with membership: invite and m.room.third_party_invite).
	// Note we'll keep external (federated) invites separate and send them after we create the room
	// as we need the room persisted first.
	externalInvites := make(map[id.UserID]*types.PartialEvent, 0)
	for _, uid := range req.Invite {
		uidStr := string(uid)
		inviteEv := types.NewPartialEvent(roomID, event.StateMember, &uidStr, userID, map[string]any{"membership": "invite"})
		if uid.Homeserver() != c.config.ServerName {
			externalInvites[uid] = inviteEv
		} else {
			evs = append(evs, inviteEv)
		}
	}

	_, err := c.db.Rooms.SendLocalEvents(r.Context(), roomID, evs, rooms.SendLocalEventsOptions{})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, fmt.Errorf("error sending local events: %w", err))
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespCreateRoom{RoomID: roomID})

	// Now send any external invites in a background goroutine so we don't block the create call
	backgroundCtx := zerolog.Ctx(r.Context()).With().
		Str("background_task", "SendRemoteInvitesAfterRoomCreate").
		Logger().
		WithContext(context.Background())
	log := zerolog.Ctx(backgroundCtx)

	c.backgroundWg.Add(1)
	go func() {
		defer c.backgroundWg.Done()

		for uid, ev := range externalInvites {
			_, respErr, err := c.prepareAndSendInviteForRemoteUser(backgroundCtx, roomID, uid, ev)
			if err != nil {
				log.Err(err).Any("resp_error", respErr).Msg("Error sending federated invite to newly created room")
				// TODO: tell the request user about this! (via their personal control room)
			}
		}
	}()
}
