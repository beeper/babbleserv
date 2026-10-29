// BabbleBot is a local user the server owns, telling local users about work done on their behalf in
// a direct message room with each, such as a join that takes a while.

package babblebot

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/accounts"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Scopes the transaction IDs of BabbleBot's notices, which deduplicate repeats
const (
	noticeDeviceID = id.DeviceID("BABBLEBOT")
	noticeEndpoint = "babblebot"
)

type Bot struct {
	log    zerolog.Logger
	config config.BabbleConfig
	db     *databases.Databases
	userID id.UserID
	exists atomic.Bool
	// A registered user has BabbleBot's localpart, so BabbleBot sends nothing rather than speak as them
	disabled atomic.Bool
}

func New(cfg config.BabbleConfig, logger zerolog.Logger, db *databases.Databases) *Bot {
	return &Bot{
		log:    logger.With().Str("component", "babblebot").Logger(),
		config: cfg,
		db:     db,
		userID: id.NewUserID(cfg.BabbleBot.Localpart, cfg.ServerName),
	}
}

func (b *Bot) EnsureUser(ctx context.Context) error {
	if b.exists.Load() || b.disabled.Load() {
		return nil
	}
	created, err := b.db.Accounts.EnsureServerUser(ctx, b.config.BabbleBot.Localpart, b.config.BabbleBot.DisplayName)
	if errors.Is(err, accounts.ErrServerUserTaken) {
		b.log.Error().Err(err).Stringer("user_id", b.userID).Msg("BabbleBot's localpart is a registered user's, disabling BabbleBot")
		b.disabled.Store(true)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to create BabbleBot's user: %w", err)
	} else if created {
		b.log.Info().Stringer("user_id", b.userID).Msg("Created BabbleBot's user")
	}
	b.exists.Store(true)
	return nil
}

// Notice sends once per transaction ID, whoever sends it, creating the user's room with BabbleBot on first use
func (b *Bot) Notice(ctx context.Context, userID id.UserID, txnID, body string) error {
	if err := b.EnsureUser(ctx); err != nil || b.disabled.Load() {
		return err
	}
	roomID, err := b.room(ctx, userID)
	if err != nil {
		return err
	}
	_, err = b.db.SendLocalEvents(ctx, roomID, []*types.PartialEvent{
		types.NewPartialEvent(roomID, event.EventMessage, nil, b.userID, map[string]any{
			"msgtype": event.MsgNotice,
			"body":    body,
		}),
	}, rooms.SendLocalEventsOptions{
		TransactionDevice:   &types.UserDevice{UserID: b.userID, DeviceID: noticeDeviceID},
		TransactionEndpoint: noticeEndpoint,
		TransactionID:       txnID,
	})
	if err != nil {
		return fmt.Errorf("failed to send BabbleBot notice: %w", err)
	}
	return nil
}

// Concurrent creators conflict on the room's record, and the others use the room the first created
func (b *Bot) room(ctx context.Context, userID id.UserID) (id.RoomID, error) {
	if roomID, err := b.db.Rooms.GetBotRoom(ctx, userID); err != nil || roomID != "" {
		return roomID, err
	}
	roomID, err := b.createRoom(ctx, userID)
	if errors.Is(err, rooms.ErrBotRoomExists) {
		return b.db.Rooms.GetBotRoom(ctx, userID)
	} else if err != nil {
		return "", fmt.Errorf("failed to create BabbleBot's room: %w", err)
	}
	if err := b.db.Accounts.AddDirectRoom(ctx, userID, b.userID, roomID); err != nil {
		b.log.Err(err).Stringer("user_id", userID).Msg("Failed to add BabbleBot's room to m.direct")
	}
	b.log.Info().Stringer("user_id", userID).Stringer("room_id", roomID).Msg("Created BabbleBot room")
	return roomID, nil
}

func (b *Bot) createRoom(ctx context.Context, userID id.UserID) (id.RoomID, error) {
	empty := ""
	botID, userKey := b.userID.String(), userID.String()
	roomVersion := b.config.Rooms.DefaultVersion
	createContent := map[string]any{"room_version": roomVersion}
	if util.RoomVersionHas(roomVersion, gomatrixserverlib.IRoomVersion.CreatorInCreateEvent) {
		createContent["creator"] = b.userID
	}
	// Creators have unlimited power from room version 12, and may not be given a level
	powerUsers := map[id.UserID]int{b.userID: 100}
	if util.RoomVersionHas(roomVersion, gomatrixserverlib.IRoomVersion.PrivilegedCreators) {
		powerUsers = map[id.UserID]int{}
	}
	createEv := types.NewPartialEvent("", event.StateCreate, &empty, b.userID, createContent)
	roomID, err := b.db.Rooms.GenerateRoomID(ctx, createEv)
	if err != nil {
		return "", err
	}
	createEv.RoomID = roomID
	evs := []*types.PartialEvent{
		createEv,
		types.NewPartialEvent(roomID, event.StateMember, &botID, b.userID, map[string]any{
			"membership":  event.MembershipJoin,
			"displayname": b.config.BabbleBot.DisplayName,
		}),
		types.NewPartialEvent(roomID, event.StatePowerLevels, &empty, b.userID, map[string]any{
			"users":          powerUsers,
			"users_default":  0,
			"events_default": 0,
			"state_default":  100,
			"invite":         100,
			"kick":           100,
			"ban":            100,
			"redact":         100,
		}),
		types.NewPartialEvent(roomID, event.StateJoinRules, &empty, b.userID, map[string]any{"join_rule": event.JoinRuleInvite}),
		types.NewPartialEvent(roomID, event.StateHistoryVisibility, &empty, b.userID, map[string]any{
			"history_visibility": event.HistoryVisibilityShared,
		}),
		types.NewPartialEvent(roomID, event.StateGuestAccess, &empty, b.userID, map[string]any{"guest_access": event.GuestAccessForbidden}),
		types.NewPartialEvent(roomID, event.StateMember, &userKey, b.userID, map[string]any{
			"membership": event.MembershipInvite,
			"is_direct":  true,
		}),
	}
	_, err = b.db.SendLocalEvents(ctx, roomID, evs, rooms.SendLocalEventsOptions{
		RequireAllEvents: true,
		BotRoomUser:      userID,
	})
	return roomID, err
}
