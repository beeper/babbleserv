package pushrules

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/types"
)

func TestPlaceRule(t *testing.T) {
	order := []string{"a", "b", "c"}

	for _, tc := range []struct {
		name, ruleID, before, after string
		expected                    []string
	}{
		{"new rule goes first", "d", "", "", []string{"d", "a", "b", "c"}},
		{"existing rule keeps position", "b", "", "", []string{"a", "b", "c"}},
		{"new rule before", "d", "b", "", []string{"a", "d", "b", "c"}},
		{"new rule after", "d", "", "c", []string{"a", "b", "c", "d"}},
		{"existing rule moved before", "c", "a", "", []string{"c", "a", "b"}},
		{"existing rule moved after", "a", "", "b", []string{"b", "a", "c"}},
		{"before takes precedence", "d", "a", "c", []string{"d", "a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placed, err := placeRule(order, tc.ruleID, tc.before, tc.after)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, placed)
			assert.Equal(t, []string{"a", "b", "c"}, order)
		})
	}

	_, err := placeRule(order, "d", ".m.rule.master", "")
	assert.ErrorIs(t, err, types.ErrPushRuleNotFound)
	_, err = placeRule(order, "a", "a", "")
	assert.ErrorIs(t, err, types.ErrPushRuleNotFound)
}

func TestMergeRules(t *testing.T) {
	defaults := pushrules.PushRuleArray{
		{RuleID: masterRuleID, Default: true},
		{RuleID: ".m.rule.suppress_notices", Default: true, Enabled: true},
	}
	stored := map[string]*pushrules.PushRule{
		masterRuleID: {RuleID: masterRuleID, Default: true, Enabled: true},
		"x":          {RuleID: "x"},
		"y":          {RuleID: "y"},
	}

	merged := mergeRules(defaults, stored, []string{"y", "x"})
	var ruleIDs []string
	for _, rule := range merged {
		ruleIDs = append(ruleIDs, rule.RuleID)
	}
	assert.Equal(t, []string{masterRuleID, "y", "x", ".m.rule.suppress_notices"}, ruleIDs)
	assert.True(t, merged[0].Enabled)

	merged = mergeRules(pushrules.PushRuleArray{{RuleID: ".m.rule.message", Default: true}}, stored, []string{"x"})
	assert.Equal(t, "x", merged[0].RuleID)
	assert.Equal(t, ".m.rule.message", merged[1].RuleID)
}
