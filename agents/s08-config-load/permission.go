package main

// Action and Rule are re-implemented locally (mirroring s04) so this module
// has zero cross-session imports — the curriculum's hard rule. The shape is
// identical to s04 / upstream's `permission/index.ts` Action + Rule:
//
//	type Rule = { permission: string; pattern: string; action: "allow"|"deny"|"ask" }
//
// In s04 these were inline literals in tests; in s08 they're loaded from
// JSON's `permissions[]` array. Anything past s08 (s09 agent registry, s10
// tool loop) reads them through Config, not by constructing them by hand.

// Action is the verdict a permission rule renders. ActionAsk is the zero
// value: an empty config or a no-match outcome both collapse to "ask the
// human" — the safe default upstream's evaluate.ts also returns.
type Action int

const (
	ActionAsk Action = iota
	ActionAllow
	ActionDeny
)

// String makes Action printable in test failures and the demo's stdout.
func (a Action) String() string {
	switch a {
	case ActionAllow:
		return "allow"
	case ActionDeny:
		return "deny"
	default:
		return "ask"
	}
}

// Rule is one row of a Ruleset. Same field layout as s04 — Permission +
// Pattern + Action — so a future s09 can hand a Config.Permissions slice
// straight to s04-style evaluate without translation.
//
// JSON tags use snake_case to match opencode's on-disk shape. We map "ask"
// (the upstream default for an unknown action string) onto ActionAsk in
// UnmarshalJSON below so unknown / missing actions don't silently allow.
type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     Action `json:"action"`
}

// UnmarshalJSON converts opencode's lowercase action strings ("allow" /
// "deny" / "ask") into the Action enum. Anything else — including missing
// — collapses to ActionAsk so a typo in opencode.json fails *closed*, not
// open. Important for the "permissions sourced from JSON" promise: if an
// editor / config-gen script garbles the action field, we don't silently
// grant access.
func (r *Rule) UnmarshalJSON(data []byte) error {
	type wire struct {
		Permission string `json:"permission"`
		Pattern    string `json:"pattern"`
		Action     string `json:"action"`
	}
	var w wire
	if err := jsonUnmarshal(data, &w); err != nil {
		return err
	}
	r.Permission = w.Permission
	r.Pattern = w.Pattern
	switch w.Action {
	case "allow":
		r.Action = ActionAllow
	case "deny":
		r.Action = ActionDeny
	default:
		r.Action = ActionAsk
	}
	return nil
}

// MarshalJSON is the symmetric encoding so the demo's `json.MarshalIndent`
// of a loaded Config prints "allow" / "deny" / "ask" instead of integers.
// The roundtrip (Marshal → Unmarshal → Marshal) is stable.
func (r Rule) MarshalJSON() ([]byte, error) {
	return jsonMarshal(struct {
		Permission string `json:"permission"`
		Pattern    string `json:"pattern"`
		Action     string `json:"action"`
	}{r.Permission, r.Pattern, r.Action.String()})
}
