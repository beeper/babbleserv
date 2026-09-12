package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/pushrules"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// parseRuleKind converts a string to a PushRuleType and validates it.
func parseRuleKind(kind string) (pushrules.PushRuleType, bool) {
	switch kind {
	case "override":
		return pushrules.OverrideRule, true
	case "content":
		return pushrules.ContentRule, true
	case "room":
		return pushrules.RoomRule, true
	case "sender":
		return pushrules.SenderRule, true
	case "underride":
		return pushrules.UnderrideRule, true
	case "postcontent":
		// TODO: this *only* exists to appease complement MSC4306
		return pushrules.PushRuleType("postcontent"), true
	default:
		return "", false
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushrules
func (c *ClientRoutes) GetPushRules(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)

	ruleset, err := c.db.Accounts.GetPushRulesForUser(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// Return the full push rules structure
	util.ResponseJSON(w, r, http.StatusOK, map[string]any{
		"global": ruleset,
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushrulesscopekind
func (c *ClientRoutes) GetPushRulesByKind(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	rules, err := c.db.Accounts.GetPushRulesForUserByKind(r.Context(), userID, kind)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, rules)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushrulesscopekindruleid
func (c *ClientRoutes) GetPushRule(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, rule)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3pushrulesscopekindruleid
func (c *ClientRoutes) PutPushRule(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	type reqType struct {
		Actions    pushrules.PushActionArray  `json:"actions"`
		Conditions []*pushrules.PushCondition `json:"conditions,omitempty"`
		Pattern    string                     `json:"pattern,omitempty"`
	}
	req, respErr := util.ParseRequestJSON[reqType](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	// Create the stored rule
	storedRule := &types.StoredPushRule{
		Actions:    req.Actions,
		Default:    false, // User-created rules are not default
		Enabled:    true,  // New rules are enabled by default
		Conditions: req.Conditions,
		Pattern:    req.Pattern,
	}

	if err := c.db.Accounts.PutPushRuleForUser(r.Context(), userID, kind, ruleID, storedRule); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#delete_matrixclientv3pushrulesscopekindruleid
func (c *ClientRoutes) DeletePushRule(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	// Check if the rule exists first
	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	if err := c.db.Accounts.DeletePushRuleForUser(r.Context(), userID, kind, ruleID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushrulesscopekindruleidenabled
func (c *ClientRoutes) GetPushRuleEnabled(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, map[string]bool{
		"enabled": rule.Enabled,
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3pushrulesscopekindruleidenabled
func (c *ClientRoutes) SetPushRuleEnabled(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	type reqType struct {
		Enabled bool `json:"enabled"`
	}
	req, respErr := util.ParseRequestJSON[reqType](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	// Get the existing rule
	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	// Update the enabled status
	storedRule := types.NewStoredPushRuleFromPushRule(rule)
	storedRule.Enabled = req.Enabled

	if err := c.db.Accounts.PutPushRuleForUser(r.Context(), userID, kind, ruleID, storedRule); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3pushrulesscopekindruleidactions
func (c *ClientRoutes) GetPushRuleActions(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, map[string]any{
		"actions": rule.Actions,
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3pushrulesscopekindruleidactions
func (c *ClientRoutes) SetPushRuleActions(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	scope := chi.URLParam(r, "scope")
	kindStr := chi.URLParam(r, "kind")
	ruleID := chi.URLParam(r, "ruleId")

	if scope != "global" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Only global scope is supported")
		return
	}

	kind, valid := parseRuleKind(kindStr)
	if !valid {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid rule kind")
		return
	}

	type reqType struct {
		Actions pushrules.PushActionArray `json:"actions"`
	}
	req, respErr := util.ParseRequestJSON[reqType](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	// Get the existing rule
	rule, err := c.db.Accounts.GetPushRuleForUser(r.Context(), userID, kind, ruleID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if rule == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	// Update the actions
	storedRule := types.NewStoredPushRuleFromPushRule(rule)
	storedRule.Actions = req.Actions

	if err := c.db.Accounts.PutPushRuleForUser(r.Context(), userID, kind, ruleID, storedRule); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
