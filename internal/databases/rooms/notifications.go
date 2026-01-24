package rooms

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// evaluateNotificationsForEvent evaluates push rules for an event against a target user.
// Returns notification deltas for this event.
//
// If a push ruleset is provided, it uses the ruleset to determine actions.
// Falls back to default behavior if ruleset is nil:
// - Count +1 notification for every message event from someone other than the target user
// - Count +1 highlight for @mentions of the target user in the message body
func evaluateNotificationsForEvent(
	ev *types.Event,
	targetUserID id.UserID,
	threadID string,
	ruleset *pushrules.PushRuleset,
	roomCtx *types.PushRuleRoom,
) types.Notifications {
	// Always ignore our own messages
	if ev.Sender == targetUserID {
		return types.Notifications{}
	}

	// If we have a ruleset, use push rules evaluation
	if ruleset != nil {
		// Convert to mautrix event for push rules
		mautrixEvt := &event.Event{
			Type:      ev.Type,
			StateKey:  ev.StateKey,
			Sender:    ev.Sender,
			RoomID:    ev.RoomID,
			ID:        ev.ID,
			Timestamp: ev.Timestamp,
			Content:   event.Content{VeryRaw: ev.Content},
		}

		// Ensure .Raw is populated as is needed for push rule eval
		json.Unmarshal(ev.Content, &mautrixEvt.Content.Raw)

		actions := ruleset.GetActions(roomCtx, mautrixEvt)
		should := actions.Should()
		if !should.Notify {
			return types.Notifications{}
		}

		notif := types.Notifications{Count: 1, ThreadID: threadID}
		if should.Highlight {
			notif.Highlight = 1
		}
		return notif
	}

	// Fallback to default behavior if no ruleset
	switch ev.Type {
	case event.EventMessage, event.EventEncrypted:
		// Only count as notification for m.room.message & m.room.encrypted
	default:
		return types.Notifications{}
	}

	notif := types.Notifications{Count: 1, ThreadID: threadID}

	mentions := ev.Mentions()
	if mentions.Room || slices.Contains(mentions.UserIDs, targetUserID) {
		// Highlight if intentional mention match
		notif.Highlight = 1
	}

	return notif
}

// txnEvaluateNotificationsForEvents evaluates notifications for a batch of events.
// Returns a map of event ID -> user ID -> notifications.
// Takes a read transaction and events directory to look up thread roots.
func txnEvaluateNotificationsForEvents(
	txn fdb.ReadTransaction,
	eventsProvider *events.TxnEventsProvider,
	evs []*types.Event,
	userPushRules map[id.UserID]*pushrules.PushRuleset,
	userRoomContext map[id.UserID]*types.PushRuleRoom,
) map[id.EventID]map[id.UserID]types.Notifications {
	result := make(map[id.EventID]map[id.UserID]types.Notifications, len(evs))

	for _, ev := range evs {
		// Skip duplicates, rejected, soft-failed, and outlier events
		if ev.IsDuplicate || ev.Rejected || ev.SoftFailed || ev.Outlier {
			continue
		}

		// Determine the thread root ID for this event
		threadID := getThreadRootID(eventsProvider, ev)

		userNotifs := make(map[id.UserID]types.Notifications, len(userPushRules))
		for userID, ruleset := range userPushRules {
			notif := evaluateNotificationsForEvent(ev, userID, threadID, ruleset, userRoomContext[userID])
			if !notif.IsEmpty() {
				userNotifs[userID] = notif
			}
		}

		if len(userNotifs) > 0 {
			result[ev.ID] = userNotifs
		}
	}

	return result
}

// getThreadRootID walks m.thread relations to find the thread root event ID.
// Returns empty string if the event is not part of a thread.
// Per Matrix spec, m.thread always points directly to the root (flat threads),
// so walking typically terminates immediately. The walking handles edge cases
// from buggy clients that might chain thread relations.
func getThreadRootID(eventsProvider *events.TxnEventsProvider, ev *types.Event) string {
	relEventID, relType := ev.RelatesTo()
	if relType != event.RelThread || relEventID == "" {
		return ""
	}

	// Walk until we find an event with no m.thread relation (the root)
	currentID := relEventID
	for {
		parentEv := eventsProvider.MustGet(currentID)
		if parentEv == nil {
			// Parent not found in our database, use current as root
			return currentID.String()
		}
		parentRelID, parentRelType := parentEv.RelatesTo()
		if parentRelType != event.RelThread || parentRelID == "" {
			// No further thread relation, this is the root
			return currentID.String()
		}
		currentID = parentRelID
	}
}

// CompactNotifications compacts notification entries for a user in a room.
// If there are 2+ entries, they are merged into a single entry with summed counts.
func (r *RoomsDatabase) CompactNotifications(ctx context.Context, userID id.UserID, roomID id.RoomID, upToVersion tuple.Versionstamp) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (struct{}, error) {
		r.users.TxnCompactNotifications(txn, userID, roomID, upToVersion)
		return struct{}{}, nil
	})
	return err
}
