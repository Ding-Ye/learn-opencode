package main

import (
	"regexp"
	"strings"
)

// Action / Rule / Evaluate — re-implemented from s04 verbatim. The
// Orchestrator below is the FIRST consumer in the curriculum that calls
// Evaluate at runtime: every tool_use Part triggers exactly one Evaluate
// call against the configured Rules slice.

// Action is the verdict a permission rule renders. ActionAsk is the zero
// value: an empty rule slice or a no-match outcome both collapse to
// "ask the human." For the test fakes, "ask" is treated as "allow"
// (interactive prompts are out of scope for s10's headless contract).
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

// Rule is one row of a permission ruleset. Order matters; later rules win.
type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     Action `json:"action"`
}

// Ruleset is a slice of Rules — one ruleset per cascade layer. s10's
// Orchestrator carries a SINGLE flat ruleset (the cascade result from
// s09's MergePermissions) so this slice IS the merged-cascade output, not
// a raw layer.
type Ruleset []Rule

// Evaluate finds the LAST rule that matches both `permission` (exact, with
// wildcard support) and `target` (wildcard glob against Rule.Pattern),
// across all `rulesets` concatenated in argument order. Returns ActionAsk
// when nothing matches.
//
// "Last match wins" — the same semantic the s04 evaluator and upstream's
// `findLast` ship. s10 uses this to ask "may we run THIS tool with THIS
// input?" before each Tool.Execute call.
func Evaluate(permission, target string, rulesets ...Ruleset) Action {
	var matched *Rule
	for ri := range rulesets {
		rs := rulesets[ri]
		for i := range rs {
			r := &rs[i]
			if !wildcardMatch(permission, r.Permission) {
				continue
			}
			if !wildcardMatch(target, r.Pattern) {
				continue
			}
			matched = r
		}
	}
	if matched == nil {
		return ActionAsk
	}
	return matched.Action
}

// wildcardMatch is a thin Go port of opencode's util/wildcard.ts `match()`.
// Same algorithm as s04's port — see s04 for the full annotated derivation.
// Compiled per call (cheap at s10's scope; a production port would memoize).
func wildcardMatch(s, pattern string) bool {
	if s != "" {
		s = strings.ReplaceAll(s, `\`, "/")
	}
	if pattern != "" {
		pattern = strings.ReplaceAll(pattern, `\`, "/")
	}

	var b strings.Builder
	b.Grow(len(pattern) * 2)
	for _, r := range pattern {
		switch r {
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteRune(r)
		}
	}
	escaped := b.String()
	if strings.HasSuffix(escaped, " .*") {
		escaped = escaped[:len(escaped)-3] + "( .*)?"
	}
	re, err := regexp.Compile("^(?s)" + escaped + "$")
	if err != nil {
		return false
	}
	return re.MatchString(s)
}
