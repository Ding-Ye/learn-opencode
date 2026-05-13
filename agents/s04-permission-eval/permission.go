package main

import (
	"regexp"
	"strings"
)

// Action is the verdict a permission rule renders on a (permission, target)
// pair. ActionAsk is the zero value on purpose: an unset Rule, an empty
// ruleset, or a no-match outcome all collapse to "ask the human" — which is
// exactly what opencode's `evaluate.ts` does (returns `{action: "ask", ...}`).
//
// Mirrors `Action` in packages/opencode/src/permission/index.ts:
//
//	export const Action = Schema.Literals(["allow", "deny", "ask"])
type Action int

const (
	// ActionAsk is the safe default: when no rule matches, ask the user.
	// Zero value so a freshly-constructed Rule{} is "ask" without any
	// initialization — the same shape opencode's `evalRule` falls back to.
	ActionAsk Action = iota

	// ActionAllow means the rule grants permission unconditionally.
	ActionAllow

	// ActionDeny means the rule blocks the action; in the loop (s10) this
	// turns into a synthetic tool_result{IsError:true} the LLM can recover from.
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

// Rule is one row of a Ruleset. Mirrors upstream's `Rule` shape:
//
//	type Rule = { permission: string; pattern: string; action: "allow"|"deny"|"ask" }
//
// Two design notes vs upstream:
//
//  1. Upstream's `pattern` field is the path/cmd glob alone — e.g. `"*.go"`,
//     `"rm -rf*"`. The "edit:" / "bash:" prefix is the `permission` field, and
//     `evaluate(permission, pattern, ...)` already takes the bare permission
//     name as its first arg. We follow that split: Rule.Pattern is the glob
//     against the target (a file path, a command line, a URL — whatever the
//     specific permission domain uses), nothing more.
//  2. We don't attach a numeric priority. Order matters; later rules win. Same
//     as upstream's `findLast`. Rulesets are flattened in cascade order
//     (defaults → user_config → agent_override) so the agent's last word wins,
//     which is the cascade s09 (agent-registry) will lean on.
type Rule struct {
	Permission string
	Pattern    string
	Action     Action
}

// Ruleset is a slice of Rules — usually sourced from one config layer
// (defaults, user opencode.json, agent override). Evaluate flattens any
// number of these in argument order, so the call site decides the cascade.
type Ruleset []Rule

// Evaluate finds the LAST rule that matches both `permission` (exact) and
// `target` (wildcard glob against Rule.Pattern), across all `rulesets`
// concatenated in argument order. Returns ActionAsk when nothing matches.
//
// "Last match wins" is load-bearing for the cascade pattern:
//
//	Evaluate("edit", "main.go", defaults, userConfig, agentOverride)
//
// — even if `defaults` allows `*.go`, an `agentOverride` rule denying
// `main.go` will win because it appears later in the flattened list.
//
// Mirrors `evaluate.ts`:
//
//	const rules = rulesets.flat()
//	const match = rules.findLast(rule =>
//	  Wildcard.match(permission, rule.permission) &&
//	  Wildcard.match(pattern,    rule.pattern))
//	return match ?? { action: "ask", permission, pattern: "*" }
//
// One Go-side simplification: upstream uses Wildcard.match for the
// permission name itself (so a Rule with `permission: "*"` matches any
// permission). We match the permission name with the same wildcard matcher,
// keeping the upstream semantics — that's how a rule like
// `{permission: "*", pattern: "*"}` becomes a global allow/deny.
func Evaluate(permission, target string, rulesets ...Ruleset) Action {
	// Walk all rulesets in order; remember the last match. We don't bail
	// early — the LATER match wins, so we have to see all of them.
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

// wildcardMatch is a thin Go port of opencode's util/wildcard.ts `match()`:
//
//   - normalize backslashes to forward slashes (Windows path tolerance);
//   - escape regex metachars in the pattern;
//   - turn `*` into `.*` and `?` into `.`;
//   - special-case the trailing " *" → "( .*)?" so `"ls *"` matches both
//     `"ls"` and `"ls -la"` (this is what lets a bash rule like
//     `git diff *` match a bare `git diff`);
//   - anchor with ^…$ and run the regex.
//
// We compile the regex per call. That's fine for s04's scope (a handful of
// rules per request); a real production port would memoize per pattern.
// upstream:packages/opencode/src/util/wildcard.ts L3-L19
func wildcardMatch(s, pattern string) bool {
	if s != "" {
		s = strings.ReplaceAll(s, `\`, "/")
	}
	if pattern != "" {
		pattern = strings.ReplaceAll(pattern, `\`, "/")
	}

	// Escape regex specials EXCEPT * and ? (which we'll translate next).
	// Same set as upstream's `/[.+^${}()|[\]\\]/g` — note `*` and `?` are
	// deliberately absent so they survive to the next replace.
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

	// Trailing " .*" → "( .*)?" so `"ls *"` matches the bare `"ls"`.
	// This is the one piece of the upstream algorithm that has no
	// equivalent in Go's stdlib — it's what makes shell-command permission
	// rules like `git diff *` ergonomic.
	if strings.HasSuffix(escaped, " .*") {
		escaped = escaped[:len(escaped)-3] + "( .*)?"
	}

	// `(?s)` is Go's equivalent of upstream's "s" flag — `.` matches `\n`
	// too, which matters for multi-line tool inputs (a shell heredoc, say).
	re, err := regexp.Compile("^(?s)" + escaped + "$")
	if err != nil {
		// A truly malformed pattern is operator error; fail closed so a
		// broken rule doesn't accidentally allow something.
		return false
	}
	return re.MatchString(s)
}
