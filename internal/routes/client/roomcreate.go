package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3createroom
func (c *ClientRoutes) CreateRoom(w http.ResponseWriter, r *http.Request) {
	var req *struct {
		mautrix.ReqCreateRoom
		PowerLevelOverride map[string]json.RawMessage `json:"power_level_content_override"`
	}
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil || req == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}

	userID := middleware.GetRequestUserID(r)
	roomID := c.db.Rooms.GenerateRoomID(r.Context())
	visibility := req.Visibility
	if visibility == "" {
		visibility = "private"
	}
	if visibility != "private" && visibility != "public" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Visibility must be public or private")
		return
	}
	if req.Preset == "" {
		if visibility == "public" {
			req.Preset = "public_chat"
		} else {
			req.Preset = "private_chat"
		}
	}
	for _, invitedUserID := range req.Invite {
		if _, _, err := invitedUserID.ParseAndValidateRelaxed(); err != nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid invite user ID")
			return
		}
	}
	if len(req.Invite3PID) > 0 {
		// Third-party invites require an identity-server token exchange and an
		// m.room.third_party_invite event. Do not silently create a room while
		// dropping the requested invites.
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "invite_3pid is not supported")
		return
	}

	evs := make([]*types.PartialEvent, 0, len(req.InitialState)+5)
	sKey := "" // blank state key to point at

	// 1: The m.room.create event itself. Must be the first event in the room.
	createContent := make(map[string]any, len(req.CreationContent)+2)
	for key, value := range req.CreationContent {
		createContent[key] = value
	}
	roomVersion := string(req.RoomVersion)
	if roomVersion == "" {
		roomVersion = c.config.Rooms.DefaultVersion
	}
	numericVersion, versionErr := strconv.Atoi(roomVersion)
	if versionErr != nil || numericVersion < 3 || numericVersion > 11 ||
		!gomatrixserverlib.KnownRoomVersion(gomatrixserverlib.RoomVersion(roomVersion)) {
		util.ResponseErrorJSON(w, r, mautrix.MUnsupportedRoomVersion)
		return
	}
	createContent["room_version"] = roomVersion
	if numericVersion < 11 {
		createContent["creator"] = userID
	} else {
		delete(createContent, "creator")
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
	powerContent := map[string]any{
		"ban":            50,
		"events_default": 0,
		"invite":         0,
		"kick":           50,
		"redact":         50,
		"state_default":  50,
		"users":          userPowerLevels,
		"users_default":  0,
		"events": map[event.Type]int{
			event.StateHistoryVisibility: 100,
			event.StatePowerLevels:       100,
			event.StateTombstone:         100,
			event.StateServerACL:         100,
		},
	}
	if req.PowerLevelOverride != nil {
		encoded, err := json.Marshal(req.PowerLevelOverride)
		var parsed event.PowerLevelsEventContent
		if err != nil || json.Unmarshal(encoded, &parsed) != nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
		for key, value := range req.PowerLevelOverride {
			powerContent[key] = value
		}
	}
	powerEv := types.NewPartialEvent(roomID, event.StatePowerLevels, &sKey, userID, powerContent)
	evs = append(evs, powerEv)

	// 4: An m.room.canonical_alias event if room_alias_name is given.
	var roomAlias id.RoomAlias
	if req.RoomAliasName != "" {
		if strings.ContainsRune(req.RoomAliasName, ':') || strings.ContainsRune(req.RoomAliasName, '\x00') {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid room alias localpart")
			return
		}
		roomAlias = id.NewRoomAlias(req.RoomAliasName, c.config.ServerName)
		if len(roomAlias) > 255 {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Room alias is too long")
			return
		}
		evs = append(evs, types.NewPartialEvent(roomID, event.StateCanonicalAlias, &sKey, userID, map[string]any{
			"alias": roomAlias.String(),
		}))
	}

	// 5: Events set by the preset. Currently these are the m.room.join_rules, m.room.history_visibility, and m.room.guest_access state events.
	preset, found := presets[req.Preset]
	if req.Preset != "" && !found {
		hlog.FromRequest(r).Warn().Msgf("Invalid create room preset: %s", req.Preset)
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid room preset")
		return
	}
	for _, ev := range preset {
		evs = append(evs, types.NewPartialEvent(roomID, ev.Type, &sKey, userID, ev.Content))
	}

	// 6: Events listed in initial_state, in the order that they are listed.
	for _, ev := range req.InitialState {
		if ev == nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Initial state events must be objects")
			return
		}
		if ev.ID != "" || ev.RoomID != "" || ev.Sender != "" {
			util.ResponseErrorMessageJSON(
				w, r, mautrix.MInvalidParam,
				"Initial state events should not contain ID, room ID or sender",
			)
			return
		}
		if ev.Type.Type == "" {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Initial state event type is required")
			return
		}
		stateKey := ev.StateKey
		if stateKey == nil {
			stateKey = &sKey
		}
		contentJSON := ev.Content.VeryRaw
		var content map[string]any
		if len(contentJSON) == 0 || json.Unmarshal(contentJSON, &content) != nil || content == nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
		evs = append(evs, types.NewPartialEvent(roomID, ev.Type, stateKey, userID, content))
	}

	// 7: Events implied by name and topic (m.room.name and m.room.topic state events).
	if req.Name != "" {
		nameEv := types.NewPartialEvent(roomID, event.StateRoomName, &sKey, userID, map[string]any{"name": req.Name})
		evs = append(evs, nameEv)
	}
	if req.Topic != "" {
		topicEv := types.NewPartialEvent(roomID, event.StateTopic, &sKey, userID, map[string]any{
			"topic": req.Topic,
			"m.topic": map[string]any{
				"m.text": []map[string]any{{
					"body": req.Topic,
				}},
			},
		})
		evs = append(evs, topicEv)
	}

	// 8: Invite events implied by invite and invite_3pid (m.room.member with membership: invite and m.room.third_party_invite).
	// Note we'll keep external (federated) invites separate and send them after we create the room
	// as we need the room persisted first.
	externalInvites := make(map[id.UserID]*types.PartialEvent, 0)
	for _, uid := range req.Invite {
		uidStr := string(uid)
		content := map[string]any{"membership": "invite"}
		if req.IsDirect {
			content["is_direct"] = true
		}
		inviteEv := types.NewPartialEvent(roomID, event.StateMember, &uidStr, userID, content)
		// TODO: invite_room_state UNSIGNED
		if uid.Homeserver() == c.config.ServerName {
			evs = append(evs, inviteEv)
		} else {
			externalInvites[uid] = inviteEv
		}
	}

	_, err := c.db.SendLocalEvents(r.Context(), roomID, evs, rooms.SendLocalEventsOptions{
		PublishRoom:      visibility == "public",
		RoomAlias:        roomAlias,
		RoomAliasOwner:   userID,
		RequireAllEvents: true,
	})
	if err != nil {
		if errors.Is(err, types.ErrRoomAliasTaken) {
			util.ResponseJSON(w, r, http.StatusBadRequest, map[string]string{
				"errcode": mautrix.MRoomInUse.ErrCode, "error": "Room alias taken",
			})
			return
		} else if errors.Is(err, rooms.ErrRequiredEventRejected) {
			util.ResponseJSON(w, r, http.StatusBadRequest, map[string]string{
				"errcode": "M_INVALID_ROOM_STATE", "error": "Initial room state is not allowed",
			})
			return
		}
		util.ResponseErrorUnknownJSON(w, r, fmt.Errorf("error sending local events: %w", err))
		return
	}

	// Remote invites are sent after the room is committed.
	backgroundCtx := zerolog.Ctx(r.Context()).With().
		Str("background_task", "SendRemoteInvitesAfterRoomCreate").
		Logger().
		WithContext(context.Background())
	log := zerolog.Ctx(backgroundCtx)

	for uid, ev := range externalInvites {
		_, respErr, err := c.prepareAndSendInviteForRemoteUser(backgroundCtx, roomID, uid, ev)
		if err != nil {
			log.Err(err).Any("resp_error", respErr).Msg("Error sending federated invite to newly created room")
			// TODO: tell the request user about this! (via their personal control room)
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespCreateRoom{RoomID: roomID})
}
