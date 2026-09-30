package pushrules

import (
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"github.com/vmihailenco/msgpack/v5"
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

	// Priority order of user-defined (non server-default) rules, most important first
	// key: (id.UserID, Scope, Kind)
	// value: []string of RuleIDs
	userPushRuleOrders subspace.Subspace

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

		userPushRules:      pushRulesDir.Sub("upr"),
		userPushRuleOrders: pushRulesDir.Sub("upo"),
		userPushVersions:   pushRulesDir.Sub("upv"),
	}
}

func (p *PushRulesDirectory) keyForRule(userID id.UserID, scope string, kind pushrules.PushRuleType, ruleID string) fdb.Key {
	return p.userPushRules.Pack(tuple.Tuple{userID.String(), scope, string(kind), ruleID})
}

func (p *PushRulesDirectory) keyForRuleOrder(userID id.UserID, scope string, kind pushrules.PushRuleType) fdb.Key {
	return p.userPushRuleOrders.Pack(tuple.Tuple{userID.String(), scope, string(kind)})
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

func (p *PushRulesDirectory) rangeForUserRuleOrders(userID id.UserID, scope string) fdb.ExactRange {
	return p.userPushRuleOrders.Sub(userID.String(), scope)
}

type storedRulesByKind map[pushrules.PushRuleType]map[string]*pushrules.PushRule

func (p *PushRulesDirectory) txnGetStoredRules(txn fdb.ReadTransaction, rng fdb.ExactRange) (storedRulesByKind, error) {
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()

	rules := make(storedRulesByKind)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// tup is (userID, scope, kind, ruleID)
		tup, err := p.userPushRules.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		kind := pushrules.PushRuleType(tup[2].(string))
		ruleID := tup[3].(string)

		if rules[kind] == nil {
			rules[kind] = make(map[string]*pushrules.PushRule)
		}
		rules[kind][ruleID] = types.MustNewStoredPushRuleFromBytes(kv.Value).ToPushRule(kind, ruleID)
	}
	return rules, nil
}

func (p *PushRulesDirectory) txnGetRuleOrders(txn fdb.ReadTransaction, userID id.UserID) (map[pushrules.PushRuleType][]string, error) {
	iter := txn.GetRange(
		p.rangeForUserRuleOrders(userID, "global"),
		fdb.RangeOptions{Mode: fdb.StreamingModeWantAll},
	).Iterator()

	orders := make(map[pushrules.PushRuleType][]string)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// tup is (userID, scope, kind)
		tup, err := p.userPushRuleOrders.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		var order []string
		if err := msgpack.Unmarshal(kv.Value, &order); err != nil {
			return nil, err
		}
		orders[pushrules.PushRuleType(tup[2].(string))] = order
	}
	return orders, nil
}

func (p *PushRulesDirectory) txnGetRuleOrder(txn fdb.ReadTransaction, userID id.UserID, kind pushrules.PushRuleType) ([]string, error) {
	value := txn.Get(p.keyForRuleOrder(userID, "global", kind)).MustGet()
	if value == nil {
		return nil, nil
	}
	var order []string
	if err := msgpack.Unmarshal(value, &order); err != nil {
		return nil, err
	}
	return order, nil
}

func (p *PushRulesDirectory) txnSetRuleOrder(txn fdb.Transaction, userID id.UserID, kind pushrules.PushRuleType, order []string) error {
	key := p.keyForRuleOrder(userID, "global", kind)
	if len(order) == 0 {
		txn.Clear(key)
		return nil
	}
	value, err := msgpack.Marshal(order)
	if err != nil {
		return err
	}
	txn.Set(key, value)
	return nil
}

func (p *PushRulesDirectory) TxnGetRulesForUser(txn fdb.ReadTransaction, userID id.UserID) (*pushrules.PushRuleset, error) {
	stored, err := p.txnGetStoredRules(txn, p.rangeForUserRules(userID, "global"))
	if err != nil {
		return nil, err
	}
	orders, err := p.txnGetRuleOrders(txn, userID)
	if err != nil {
		return nil, err
	}

	ruleset := DefaultPushRuleset(userID)
	ruleset.Override = mergeRules(ruleset.Override, stored[pushrules.OverrideRule], orders[pushrules.OverrideRule])
	ruleset.Content = mergeRules(ruleset.Content, stored[pushrules.ContentRule], orders[pushrules.ContentRule])
	ruleset.Room = mergeRules(ruleset.Room.Unmap(), stored[pushrules.RoomRule], orders[pushrules.RoomRule]).
		SetTypeAndMap(pushrules.RoomRule)
	ruleset.Sender = mergeRules(ruleset.Sender.Unmap(), stored[pushrules.SenderRule], orders[pushrules.SenderRule]).
		SetTypeAndMap(pushrules.SenderRule)
	ruleset.Underride = mergeRules(ruleset.Underride, stored[pushrules.UnderrideRule], orders[pushrules.UnderrideRule])

	return ruleset, nil
}

const masterRuleID = ".m.rule.master"

// mergeRules applies stored changes to server-default rules and inserts the user-defined
// rules, in priority order, ahead of the server-default rules. The exception is
// .m.rule.master which always remains the highest priority rule.
func mergeRules(defaults pushrules.PushRuleArray, stored map[string]*pushrules.PushRule, order []string) pushrules.PushRuleArray {
	merged := make(pushrules.PushRuleArray, 0, len(defaults)+len(order))
	for _, rule := range defaults {
		if storedRule, ok := stored[rule.RuleID]; ok {
			rule = storedRule
		}
		merged = append(merged, rule)
	}

	userRules := make(pushrules.PushRuleArray, 0, len(order))
	for _, ruleID := range order {
		if rule, ok := stored[ruleID]; ok {
			userRules = append(userRules, rule)
		}
	}

	insertAt := 0
	if len(merged) > 0 && merged[0].RuleID == masterRuleID {
		insertAt = 1
	}
	return slices.Insert(merged, insertAt, userRules...)
}

// placeRule positions ruleID within a user-defined rule order following the PUT
// pushrules before/after semantics, before taking precedence. Without either an
// existing rule keeps its position and a new rule becomes the most important.
func placeRule(order []string, ruleID, before, after string) ([]string, error) {
	if before == "" && after == "" {
		if slices.Contains(order, ruleID) {
			return order, nil
		}
		return slices.Insert(order, 0, ruleID), nil
	}

	order = slices.DeleteFunc(slices.Clone(order), func(id string) bool { return id == ruleID })

	target, offset := before, 0
	if target == "" {
		target, offset = after, 1
	}
	idx := slices.Index(order, target)
	if idx < 0 {
		return nil, types.ErrPushRuleNotFound
	}
	return slices.Insert(order, idx+offset, ruleID), nil
}

func (p *PushRulesDirectory) TxnGetRulesForUserByKind(txn fdb.ReadTransaction, userID id.UserID, kind pushrules.PushRuleType) ([]*pushrules.PushRule, error) {
	stored, err := p.txnGetStoredRules(txn, p.rangeForUserKindRules(userID, "global", kind))
	if err != nil {
		return nil, err
	}
	order, err := p.txnGetRuleOrder(txn, userID, kind)
	if err != nil {
		return nil, err
	}

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
	}

	return mergeRules(rules, stored[kind], order), nil
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

func (p *PushRulesDirectory) TxnPutRuleForUser(
	txn fdb.Transaction,
	userID id.UserID,
	kind pushrules.PushRuleType,
	ruleID string,
	rule *types.StoredPushRule,
	before, after string,
) error {
	if !rule.Default {
		order, err := p.txnGetRuleOrder(txn, userID, kind)
		if err != nil {
			return err
		}
		order, err = placeRule(order, ruleID, before, after)
		if err != nil {
			return err
		}
		if err := p.txnSetRuleOrder(txn, userID, kind, order); err != nil {
			return err
		}
	}

	key := p.keyForRule(userID, "global", kind, ruleID)
	txn.Set(key, rule.ToBytes())

	// Update the user's push rules version
	p.txnUpdateUserPushVersion(txn, userID)
	return nil
}

// TxnDeleteRuleForUser deletes a user-defined rule, server-default rules cannot be
// deleted and return types.ErrPushRuleNotFound.
func (p *PushRulesDirectory) TxnDeleteRuleForUser(txn fdb.Transaction, userID id.UserID, kind pushrules.PushRuleType, ruleID string) error {
	key := p.keyForRule(userID, "global", kind, ruleID)
	value := txn.Get(key).MustGet()
	if value == nil || types.MustNewStoredPushRuleFromBytes(value).Default {
		return types.ErrPushRuleNotFound
	}
	txn.Clear(key)

	order, err := p.txnGetRuleOrder(txn, userID, kind)
	if err != nil {
		return err
	}
	order = slices.DeleteFunc(order, func(id string) bool { return id == ruleID })
	if err := p.txnSetRuleOrder(txn, userID, kind, order); err != nil {
		return err
	}

	// Update the user's push rules version
	p.txnUpdateUserPushVersion(txn, userID)
	return nil
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
