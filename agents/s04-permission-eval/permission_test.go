package main

import "testing"

// TestEmptyRulesetsAsk pins the default contract: with no rules in scope,
// Evaluate must return ActionAsk. This is the "safe by default" guarantee —
// if the s10 loop calls Evaluate with no config loaded, the human gets
// prompted instead of silently allowing or denying.
//
// Mirrors upstream's `return match ?? { action: "ask", ... }` fallback.
func TestEmptyRulesetsAsk(t *testing.T) {
	if got := Evaluate("edit", "main.go"); got != ActionAsk {
		t.Errorf("Evaluate(no rulesets) = %v, want %v", got, ActionAsk)
	}
	// And with empty rulesets passed explicitly — same result.
	if got := Evaluate("edit", "main.go", Ruleset{}, Ruleset{}); got != ActionAsk {
		t.Errorf("Evaluate(empty rulesets) = %v, want %v", got, ActionAsk)
	}
}

// TestSingleAllowRule is the smallest happy path: one rule, exact permission
// match, pattern matches the target → ActionAllow. If this fails, the basic
// match-and-return wiring is broken and every other test is meaningless.
func TestSingleAllowRule(t *testing.T) {
	rs := Ruleset{
		{Permission: "edit", Pattern: "main.go", Action: ActionAllow},
	}
	if got := Evaluate("edit", "main.go", rs); got != ActionAllow {
		t.Errorf("Evaluate(edit, main.go) = %v, want %v", got, ActionAllow)
	}
	// Wrong permission name → no match, fall through to ask.
	if got := Evaluate("bash", "main.go", rs); got != ActionAsk {
		t.Errorf("Evaluate(bash, main.go) = %v, want %v (permission mismatch)", got, ActionAsk)
	}
}

// TestLaterDenyOverridesEarlierAllow is the load-bearing assertion of the
// whole module: last-match-wins. An earlier `allow *.go` followed by a later
// `deny main.go` must resolve to deny — that's how the cascade pattern in s09
// (defaults → user_config → agent_override) lets each layer override the
// previous one without rewriting earlier rulesets.
//
// Mirrors upstream's `Array.findLast` semantics in evaluate.ts L11.
func TestLaterDenyOverridesEarlierAllow(t *testing.T) {
	rs := Ruleset{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow}, // earlier: allow all .go
		{Permission: "edit", Pattern: "main.go", Action: ActionDeny}, // later:   deny main.go
	}
	if got := Evaluate("edit", "main.go", rs); got != ActionDeny {
		t.Errorf("Evaluate(edit, main.go) = %v, want %v (later deny must win)", got, ActionDeny)
	}
	// And a sibling .go file the deny doesn't touch — the earlier allow still wins.
	if got := Evaluate("edit", "lib.go", rs); got != ActionAllow {
		t.Errorf("Evaluate(edit, lib.go) = %v, want %v", got, ActionAllow)
	}
}

// TestPatternGlob exercises the wildcard matcher itself: `*.go` should match
// `main.go` (any string before the literal `.go`) but NOT `main.py`. If this
// fails, every glob-shaped rule in the codebase is a footgun.
func TestPatternGlob(t *testing.T) {
	rs := Ruleset{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow},
	}
	if got := Evaluate("edit", "main.go", rs); got != ActionAllow {
		t.Errorf("Evaluate(edit, main.go) = %v, want %v", got, ActionAllow)
	}
	if got := Evaluate("edit", "main.py", rs); got != ActionAsk {
		t.Errorf("Evaluate(edit, main.py) = %v, want %v (.py shouldn't match *.go)", got, ActionAsk)
	}
	// Empty target against `*` is also a useful corner — opencode's wildcard
	// matcher allows zero-or-more, so `*` matches the empty string.
	rsStar := Ruleset{
		{Permission: "edit", Pattern: "*", Action: ActionAllow},
	}
	if got := Evaluate("edit", "", rsStar); got != ActionAllow {
		t.Errorf("Evaluate(edit, '') against `*` = %v, want %v", got, ActionAllow)
	}
}

// TestMultiRulesetCascade is the integration assertion: three rulesets
// (defaults → user_config → agent_override), and the agent's deny on a
// specific path must override the user's broad allow.
//
// This is exactly the call shape s09 (agent-registry) will use when a tool
// dispatch needs a permission verdict:
//
//	Evaluate("edit", path, builtinDefaults, configFromOpencodeJSON, agent.Permissions)
func TestMultiRulesetCascade(t *testing.T) {
	defaults := Ruleset{
		{Permission: "edit", Pattern: "*", Action: ActionAsk}, // built-in: ask for any edit
	}
	userConfig := Ruleset{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow}, // user trusts all .go edits
	}
	agentOverride := Ruleset{
		{Permission: "edit", Pattern: "secrets.go", Action: ActionDeny}, // agent locks down secrets.go
	}

	// Generic .go file: defaults says ask, user says allow, agent says nothing → allow wins.
	if got := Evaluate("edit", "main.go", defaults, userConfig, agentOverride); got != ActionAllow {
		t.Errorf("Evaluate(edit, main.go) cascade = %v, want %v (user's allow wins)", got, ActionAllow)
	}
	// secrets.go: defaults ask, user allow, agent deny → deny wins (last).
	if got := Evaluate("edit", "secrets.go", defaults, userConfig, agentOverride); got != ActionDeny {
		t.Errorf("Evaluate(edit, secrets.go) cascade = %v, want %v (agent's deny wins)", got, ActionDeny)
	}
	// Non-go file: only defaults's `*` matches → ask.
	if got := Evaluate("edit", "README.md", defaults, userConfig, agentOverride); got != ActionAsk {
		t.Errorf("Evaluate(edit, README.md) cascade = %v, want %v (only defaults matches)", got, ActionAsk)
	}
}
