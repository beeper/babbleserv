package pushrules

import (
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"
)

// DefaultPushRuleset returns the predefined push rules as specified in the Matrix spec.
// https://spec.matrix.org/v1.13/client-server-api/#predefined-rules
func DefaultPushRuleset(userID id.UserID) *pushrules.PushRuleset {
	// Common action arrays
	notifyDefault := pushrules.PushActionArray{
		{Action: pushrules.ActionNotify},
		{Action: pushrules.ActionSetTweak, Tweak: pushrules.TweakSound, Value: "default"},
	}
	notifyHighlight := pushrules.PushActionArray{
		{Action: pushrules.ActionNotify},
		{Action: pushrules.ActionSetTweak, Tweak: pushrules.TweakHighlight},
	}
	notifyHighlightDefault := pushrules.PushActionArray{
		{Action: pushrules.ActionNotify},
		{Action: pushrules.ActionSetTweak, Tweak: pushrules.TweakSound, Value: "default"},
		{Action: pushrules.ActionSetTweak, Tweak: pushrules.TweakHighlight},
	}
	notifyRing := pushrules.PushActionArray{
		{Action: pushrules.ActionNotify},
		{Action: pushrules.ActionSetTweak, Tweak: pushrules.TweakSound, Value: "ring"},
	}
	dontNotify := pushrules.PushActionArray{}

	return &pushrules.PushRuleset{
		Override: pushrules.PushRuleArray{
			// .m.rule.master - disables all notifications when enabled (disabled by default)
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.master",
				Default: true,
				Enabled: false,
				Actions: dontNotify,
			},
			// .m.rule.suppress_notices - suppress notifications for m.notice messages
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.suppress_notices",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "content.msgtype", Pattern: "m.notice"},
				},
				Actions: dontNotify,
			},
			// .m.rule.invite_for_me - notify with sound when receiving room invitation
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.invite_for_me",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.member"},
					{Kind: pushrules.KindEventMatch, Key: "content.membership", Pattern: "invite"},
					{Kind: pushrules.KindEventMatch, Key: "state_key", Pattern: string(userID)},
				},
				Actions: notifyDefault,
			},
			// .m.rule.member_event - suppress notifications for all membership events
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.member_event",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.member"},
				},
				Actions: dontNotify,
			},
			// .m.rule.is_user_mention - notify with sound and highlight when mentioned
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.is_user_mention",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventPropertyContains, Key: "content.m\\.mentions.user_ids", Value: string(userID)},
				},
				Actions: notifyHighlightDefault,
			},
			// .m.rule.is_room_mention - notify with highlight for room mentions
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.is_room_mention",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventPropertyIs, Key: "content.m\\.mentions.room", Value: true},
					{Kind: pushrules.KindSenderNotificationPermission, Key: "room"},
				},
				Actions: notifyHighlight,
			},
			// .m.rule.tombstone - notify with highlight when room is upgraded
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.tombstone",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.tombstone"},
					{Kind: pushrules.KindEventMatch, Key: "state_key", Pattern: ""},
				},
				Actions: notifyHighlight,
			},
			// .m.rule.reaction - suppress notifications for reactions
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.reaction",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.reaction"},
				},
				Actions: dontNotify,
			},
			// .m.rule.room.server_acl - suppress notifications for server ACL events
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.room.server_acl",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.server_acl"},
					{Kind: pushrules.KindEventMatch, Key: "state_key", Pattern: ""},
				},
				Actions: dontNotify,
			},
			// .m.rule.suppress_edits - suppress notifications for message edits
			{
				Type:    pushrules.OverrideRule,
				RuleID:  ".m.rule.suppress_edits",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventPropertyIs, Key: "content.m\\.relates_to.rel_type", Value: "m.replace"},
				},
				Actions: dontNotify,
			},
		},
		Content: pushrules.PushRuleArray{},
		Room:    pushrules.PushRuleMap{Map: make(map[string]*pushrules.PushRule), Type: pushrules.RoomRule},
		Sender:  pushrules.PushRuleMap{Map: make(map[string]*pushrules.PushRule), Type: pushrules.SenderRule},
		Underride: pushrules.PushRuleArray{
			// .m.rule.call - notify with ring sound for incoming calls
			{
				Type:    pushrules.UnderrideRule,
				RuleID:  ".m.rule.call",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.call.invite"},
				},
				Actions: notifyRing,
			},
			// .m.rule.encrypted_room_one_to_one - notify for encrypted messages in DMs
			{
				Type:    pushrules.UnderrideRule,
				RuleID:  ".m.rule.encrypted_room_one_to_one",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindRoomMemberCount, MemberCountCondition: "2"},
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.encrypted"},
				},
				Actions: notifyDefault,
			},
			// .m.rule.room_one_to_one - notify for messages in DMs
			{
				Type:    pushrules.UnderrideRule,
				RuleID:  ".m.rule.room_one_to_one",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindRoomMemberCount, MemberCountCondition: "2"},
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.message"},
				},
				Actions: notifyDefault,
			},
			// .m.rule.message - notify for all messages
			{
				Type:    pushrules.UnderrideRule,
				RuleID:  ".m.rule.message",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.message"},
				},
				Actions: pushrules.PushActionArray{{Action: pushrules.ActionNotify}},
			},
			// .m.rule.encrypted - notify for all encrypted messages in group rooms
			{
				Type:    pushrules.UnderrideRule,
				RuleID:  ".m.rule.encrypted",
				Default: true,
				Enabled: true,
				Conditions: []*pushrules.PushCondition{
					{Kind: pushrules.KindEventMatch, Key: "type", Pattern: "m.room.encrypted"},
				},
				Actions: pushrules.PushActionArray{{Action: pushrules.ActionNotify}},
			},
		},
	}
}
