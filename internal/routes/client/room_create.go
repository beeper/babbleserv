package client

import (
	"encoding/json"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const defaultRoomVersion = "11"

var presets map[string][]types.Event

func init() {
	sKey := ""
	presets = map[string][]types.Event{
		"private_chat": {
			*types.NewEvent("", event.StateJoinRules, &sKey, "", map[string]any{"join_rule": event.JoinRuleInvite}),
			*types.NewEvent("", event.StateHistoryVisibility, &sKey, "", map[string]any{"join_rule": event.HistoryVisibilityShared}),
			*types.NewEvent("", event.StateGuestAccess, &sKey, "", map[string]any{"join_rule": event.GuestAccessCanJoin}),
		},
		"trusted_private_chat": {
			*types.NewEvent("", event.StateJoinRules, &sKey, "", map[string]any{"join_rule": event.JoinRuleInvite}),
			*types.NewEvent("", event.StateHistoryVisibility, &sKey, "", map[string]any{"join_rule": event.HistoryVisibilityShared}),
			*types.NewEvent("", event.StateGuestAccess, &sKey, "", map[string]any{"join_rule": event.GuestAccessCanJoin}),
		},
		"public_chat": {
			*types.NewEvent("", event.StateJoinRules, &sKey, "", map[string]any{"join_rule": event.JoinRulePublic}),
			*types.NewEvent("", event.StateHistoryVisibility, &sKey, "", map[string]any{"join_rule": event.HistoryVisibilityShared}),
			*types.NewEvent("", event.StateGuestAccess, &sKey, "", map[string]any{"join_rule": event.GuestAccessForbidden}),
		},
	}
}

// https://spec.matrix.org/v1.10/client-server-api/#post_matrixclientv3createroom
func (c *ClientRoutes) CreateRoom(w http.ResponseWriter, r *http.Request) {
	var req mautrix.ReqCreateRoom
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	user := middleware.GetRequestUser(r)
	userID := user.UserID("localhost") // TODO: hostname

	roomID, err := c.db.Rooms.GenerateRoomID()
	if err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to create room ID")
		return
	}

	evs := make([]*types.Event, 0, len(req.InitialState)+5)
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
		createContent["version"] = defaultRoomVersion
	} else {
		createContent["version"] = req.RoomVersion
	}
	createEv := types.NewEvent(roomID, event.StateCreate, &sKey, userID, createContent)
	evs = append(evs, createEv)

	// 2: An m.room.member event for the creator to join the room. This is needed so the remaining events can be sent.
	userIDStr := string(userID)
	creatorEv := types.NewEvent(roomID, event.StateMember, &userIDStr, userID, map[string]any{
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
	powerEv := types.NewEvent(roomID, event.StatePowerLevels, &sKey, userID, map[string]any{
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
		ev.RoomID = roomID
		ev.Sender = userID
		evs = append(evs, &ev)
	}

	// 6: Events listed in initial_state, in the order that they are listed.
	for _, ev := range req.InitialState {
		evs = append(evs, types.NewEventFromMautrix(ev))
	}

	// 7: Events implied by name and topic (m.room.name and m.room.topic state events).
	if req.Name != "" {
		nameEv := types.NewEvent(roomID, event.StateRoomName, &sKey, userID, map[string]any{"name": req.Name})
		evs = append(evs, nameEv)
	}
	if req.Topic != "" {
		topicEv := types.NewEvent(roomID, event.StateTopic, &sKey, userID, map[string]any{"name": req.Topic})
		evs = append(evs, topicEv)
	}

	// 8: Invite events implied by invite and invite_3pid (m.room.member with membership: invite and m.room.third_party_invite).
	for _, uid := range req.Invite {
		uidStr := string(uid)
		inviteEv := types.NewEvent(roomID, event.StateMember, &uidStr, userID, map[string]any{"membership": "invite"})
		evs = append(evs, inviteEv)
	}

	// Send it!
	_, err = c.db.Rooms.SendLocalEvents(r.Context(), roomID, evs)
	if err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to store new room events")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespCreateRoom{RoomID: roomID})
}
