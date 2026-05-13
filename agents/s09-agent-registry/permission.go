package main

// Action and Rule are re-implemented locally (mirroring s04 and s08) so this
// module has zero cross-session imports — the curriculum's hard rule. The
// shape is identical to s04 / upstream's `permission/index.ts` Action + Rule:
//
//	type Rule = { permission: string; pattern: string; action: "allow"|"deny"|"ask" }
//
// In s04 these were inline literals in tests; in s08 they were loaded from
// JSON's `permissions[]` array. In s09 they're held inside an Agent and
// MERGED across three layers (defaults → user config → agent override) by
// MergePermissions in agent.go. Anything past s09 (s10 tool loop) reads
// the cascaded result through Agent.Permissions.

// Action is the verdict a permission rule renders. ActionAsk is the zero
// value: an empty rule list, or a no-match outcome at evaluate time, both
// collapse to "ask the human" — the safe default upstream's evaluate.ts
// also returns.
type Action int

const (
	// ActionAsk is the safe default — see the iota comment above.
	ActionAsk Action = iota
	// ActionAllow means the rule grants permission unconditionally.
	ActionAllow
	// ActionDeny means the rule blocks the action; in s10's loop this turns
	// into a synthetic tool_result{IsError:true} the LLM can recover from.
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

// Rule is one row of a permission ruleset. Mirrors upstream's `Rule` shape:
//
//	type Rule = { permission: string; pattern: string; action: "allow"|"deny"|"ask" }
//
// As in s04, ordering matters; later wins. MergePermissions concatenates the
// three cascade layers in (defaults, userConfig, agentOverride) order so an
// agent's last word always overrides the broader defaults. We do not call
// Evaluate here — s09's job is to *produce* the merged ruleset; s10 will
// hand it to a last-match-wins evaluator (the s04 contract).
type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     Action `json:"action"`
}
