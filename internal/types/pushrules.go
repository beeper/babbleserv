package types

import (
	"github.com/vmihailenco/msgpack/v5"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"
)

// StoredPushRule is the stored version of a push rule that excludes Type and RuleID
// since those are stored in the key.
type StoredPushRule struct {
	Actions    pushrules.PushActionArray  `msgpack:"ac" json:"actions"`
	Default    bool                       `msgpack:"de" json:"default"`
	Enabled    bool                       `msgpack:"en" json:"enabled"`
	Conditions []*pushrules.PushCondition `msgpack:"co" json:"conditions,omitempty"`
	Pattern    string                     `msgpack:"pa" json:"pattern,omitempty"`
}

func NewStoredPushRuleFromBytes(b []byte) (*StoredPushRule, error) {
	var s StoredPushRule
	if err := msgpack.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func MustNewStoredPushRuleFromBytes(b []byte) *StoredPushRule {
	return exerrors.Must(NewStoredPushRuleFromBytes(b))
}

func (s *StoredPushRule) ToBytes() []byte {
	if b, err := msgpack.Marshal(s); err != nil {
		panic(err)
	} else {
		return b
	}
}

func NewStoredPushRuleFromPushRule(rule *pushrules.PushRule) *StoredPushRule {
	return &StoredPushRule{
		Actions:    rule.Actions,
		Default:    rule.Default,
		Enabled:    rule.Enabled,
		Conditions: rule.Conditions,
		Pattern:    rule.Pattern,
	}
}

func (s *StoredPushRule) ToPushRule(kind pushrules.PushRuleType, ruleID string) *pushrules.PushRule {
	return &pushrules.PushRule{
		Type:       kind,
		RuleID:     ruleID,
		Actions:    s.Actions,
		Default:    s.Default,
		Enabled:    s.Enabled,
		Conditions: s.Conditions,
		Pattern:    s.Pattern,
	}
}

// PushRuleRoom implements the pushrules.Room interface for push rule evaluation
type PushRuleRoom struct {
	MemberCount    int
	OwnDisplayname string
}

func (r *PushRuleRoom) GetOwnDisplayname() string { return r.OwnDisplayname }
func (r *PushRuleRoom) GetMemberCount() int       { return r.MemberCount }

type UserPushRulesMap map[id.UserID]*pushrules.PushRuleset
type UserRoomContextMap map[id.UserID]*PushRuleRoom
