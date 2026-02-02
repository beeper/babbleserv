package pushrules

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/types"
)

type PushRulesDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// Push rules storage
	// key: (id.UserID, Scope, Kind, RuleID)
	// value: types.StoredPushRule
	// Scope is "global" in Matrix spec (reserved for future scopes)
	// Kind is one of: override, content, room, sender, underride
	userPushRules subspace.Subspace

	// Version tracking for sync
	// key: (id.UserID)
	// value: tuple.Versionstamp
	userPushVersions subspace.Subspace
}

func NewPushRulesDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *PushRulesDirectory {
	pushRulesDir, err := parentDir.CreateOrOpen(db, []string{"pushrules"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "pushrules").Logger()
	log.Debug().
		Bytes("prefix", pushRulesDir.Bytes()).
		Msg("Init accounts/pushrules directory")

	return &PushRulesDirectory{
		log: log,
		db:  db,

		userPushRules:    pushRulesDir.Sub("upr"),
		userPushVersions: pushRulesDir.Sub("upv"),
	}
}

func (p *PushRulesDirectory) keyForRule(userID id.UserID, scope string, kind pushrules.PushRuleType, ruleID string) fdb.Key {
	return p.userPushRules.Pack(tuple.Tuple{userID.String(), scope, string(kind), ruleID})
}

func (p *PushRulesDirectory) keyForUserPushVersion(userID id.UserID) fdb.Key {
	return p.userPushVersions.Pack(tuple.Tuple{userID.String()})
}

func (p *PushRulesDirectory) rangeForUserRules(userID id.UserID, scope string) fdb.ExactRange {
	return p.userPushRules.Sub(userID.String(), scope)
}

func (p *PushRulesDirectory) rangeForUserKindRules(userID id.UserID, scope string, kind pushrules.PushRuleType) fdb.ExactRange {
	return p.userPushRules.Sub(userID.String(), scope, string(kind))
}

func (p *PushRulesDirectory) TxnGetRulesForUser(txn fdb.ReadTransaction, userID id.UserID) (*pushrules.PushRuleset, error) {
	rng := p.rangeForUserRules(userID, "global")
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()

	// Start with default rules
	ruleset := DefaultPushRuleset(userID)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// Unpack the key to get kind and ruleID
		tup, err := p.userPushRules.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		// tup is (userID, scope, kind, ruleID)
		kind := pushrules.PushRuleType(tup[2].(string))
		ruleID := tup[3].(string)

		storedRule := types.MustNewStoredPushRuleFromBytes(kv.Value)
		rule := storedRule.ToPushRule(kind, ruleID)

		switch kind {
		case pushrules.OverrideRule:
			ruleset.Override = mergeRule(ruleset.Override, rule)
		case pushrules.ContentRule:
			ruleset.Content = mergeRule(ruleset.Content, rule)
		case pushrules.RoomRule:
			ruleset.Room.Map[ruleID] = rule
		case pushrules.SenderRule:
			ruleset.Sender.Map[ruleID] = rule
		case pushrules.UnderrideRule:
			ruleset.Underride = mergeRule(ruleset.Underride, rule)
		}
	}

	return ruleset, nil
}

// mergeRule updates an existing rule if it exists, otherwise appends the rule
func mergeRule(rules pushrules.PushRuleArray, rule *pushrules.PushRule) pushrules.PushRuleArray {
	for i, existing := range rules {
		if existing.RuleID == rule.RuleID {
			rules[i] = rule
			return rules
		}
	}
	return append(rules, rule)
}

func (p *PushRulesDirectory) TxnGetRulesForUserByKind(txn fdb.ReadTransaction, userID id.UserID, kind pushrules.PushRuleType) ([]*pushrules.PushRule, error) {
	rng := p.rangeForUserKindRules(userID, "global", kind)
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()

	// Start with default rules for this kind
	defaultRuleset := DefaultPushRuleset(userID)
	var rules pushrules.PushRuleArray
	switch kind {
	case pushrules.OverrideRule:
		rules = defaultRuleset.Override
	case pushrules.ContentRule:
		rules = defaultRuleset.Content
	case pushrules.RoomRule:
		rules = defaultRuleset.Room.Unmap()
	case pushrules.SenderRule:
		rules = defaultRuleset.Sender.Unmap()
	case pushrules.UnderrideRule:
		rules = defaultRuleset.Underride
	default:
		rules = make(pushrules.PushRuleArray, 0)
	}

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// Unpack the key to get ruleID
		tup, err := p.userPushRules.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		// tup is (userID, scope, kind, ruleID)
		ruleID := tup[3].(string)

		storedRule := types.MustNewStoredPushRuleFromBytes(kv.Value)
		rules = mergeRule(rules, storedRule.ToPushRule(kind, ruleID))
	}

	return rules, nil
}

func (p *PushRulesDirectory) TxnGetRuleForUser(txn fdb.ReadTransaction, userID id.UserID, kind pushrules.PushRuleType, ruleID string) (*pushrules.PushRule, error) {
	key := p.keyForRule(userID, "global", kind, ruleID)
	value := txn.Get(key).MustGet()
	if value == nil {
		// No user-defined rule, check for default rule
		return getDefaultRule(userID, kind, ruleID), nil
	}

	storedRule := types.MustNewStoredPushRuleFromBytes(value)
	return storedRule.ToPushRule(kind, ruleID), nil
}

func getDefaultRule(userID id.UserID, kind pushrules.PushRuleType, ruleID string) *pushrules.PushRule {
	defaultRuleset := DefaultPushRuleset(userID)
	var rules pushrules.PushRuleArray
	switch kind {
	case pushrules.OverrideRule:
		rules = defaultRuleset.Override
	case pushrules.ContentRule:
		rules = defaultRuleset.Content
	case pushrules.RoomRule:
		if rule, ok := defaultRuleset.Room.Map[ruleID]; ok {
			return rule
		}
		return nil
	case pushrules.SenderRule:
		if rule, ok := defaultRuleset.Sender.Map[ruleID]; ok {
			return rule
		}
		return nil
	case pushrules.UnderrideRule:
		rules = defaultRuleset.Underride
	default:
		return nil
	}

	for _, rule := range rules {
		if rule.RuleID == ruleID {
			return rule
		}
	}
	return nil
}

func (p *PushRulesDirectory) TxnPutRuleForUser(txn fdb.Transaction, userID id.UserID, kind pushrules.PushRuleType, ruleID string, rule *types.StoredPushRule) {
	key := p.keyForRule(userID, "global", kind, ruleID)
	txn.Set(key, rule.ToBytes())

	// Update the user's push rules version
	p.txnUpdateUserPushVersion(txn, userID)
}

func (p *PushRulesDirectory) TxnDeleteRuleForUser(txn fdb.Transaction, userID id.UserID, kind pushrules.PushRuleType, ruleID string) {
	key := p.keyForRule(userID, "global", kind, ruleID)
	txn.Clear(key)

	// Update the user's push rules version
	p.txnUpdateUserPushVersion(txn, userID)
}

func (p *PushRulesDirectory) txnUpdateUserPushVersion(txn fdb.Transaction, userID id.UserID) {
	key := p.keyForUserPushVersion(userID)
	version := tuple.IncompleteVersionstamp(0)
	versionBytes := types.MustVersionstampToBytes(version)
	txn.SetVersionstampedValue(key, versionBytes)
}

func (p *PushRulesDirectory) TxnGetUserPushVersion(txn fdb.ReadTransaction, userID id.UserID) tuple.Versionstamp {
	key := p.keyForUserPushVersion(userID)
	value := txn.Get(key).MustGet()
	if value == nil {
		return types.ZeroVersionstamp
	}
	return types.MustBytesToVersionstamp(value)
}
