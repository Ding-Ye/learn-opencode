package main

import (
	"errors"
	"fmt"
	"sort"
)

// Mode is the deployment shape an Agent claims. It's the upstream
// `Schema.Literals(["subagent", "primary", "all"])` from
// `packages/opencode/src/agent/agent.ts` L31, mapped to a Go enum so
// ListByMode can filter without string compares.
//
//   - ModePrimary — the user picks this agent at session start (build, plan, …)
//   - ModeSubagent — invoked by another agent via the task tool (general, …)
//   - ModeAll — eligible in either context; the catch-all for user-defined
//     agents that don't declare a preference.
type Mode int

const (
	ModePrimary Mode = iota
	ModeSubagent
	ModeAll
)

// String makes Mode printable in test failures and the demo's stdout.
func (m Mode) String() string {
	switch m {
	case ModePrimary:
		return "primary"
	case ModeSubagent:
		return "subagent"
	case ModeAll:
		return "all"
	default:
		return "unknown"
	}
}

// Agent is the runtime bundle of (mode, model, system prompt, permissions,
// tool whitelist) keyed by Name. Mirrors upstream's `Info` Schema in
// agent.ts L28-L48 — same five load-bearing fields, simplified by dropping
// the rendering-only ones (color / temperature / topP / variant / steps /
// hidden / native / options) that don't change behavior.
//
// Tools is the optional tool-name whitelist (upstream's `tools` field on
// the agent shape). Empty means "any tool the registry exposes" — the
// upstream default. A non-empty list filters s10's loop to only those tools.
//
// Permissions is the *result* of the three-layer cascade
// (defaults → userConfig → agentOverride). NewRegistry's built-ins fill it
// in eagerly; user-defined agents passed to Register fill it in themselves
// (typically via MergePermissions). Either way, by the time an Agent lands
// in the registry, this slice is the final last-match-wins ruleset.
type Agent struct {
	Name        string
	Mode        Mode
	Model       string
	System      string
	Permissions []Rule
	Tools       []string
}

// Registry holds the runtime catalog of named agents. Built-ins ("build",
// "plan", "general") are pre-populated by NewRegistry; user-defined agents
// are added via Register, which silently overrides any built-in of the same
// name (mirroring upstream's behavior at agent.ts L282-L304 where each entry
// in `cfg.agent` either patches an existing entry or installs a new one).
//
// The map is unsorted; ListByMode returns its results sorted by Name so
// tests and the demo are deterministic.
type Registry struct {
	agents map[string]*Agent
}

// NewRegistry builds a Registry seeded with three built-in agents that
// mirror the canonical opencode set:
//
//   - "build"   (ModePrimary)  — wide-open: the default that runs any tool.
//   - "plan"    (ModePrimary)  — read-only: edit/bash/write denied;
//     read/grep/glob allowed. Mirrors upstream's `plan` agent at
//     agent.ts L139-L161 (which denies edit/* and allows the planning paths).
//   - "general" (ModeAll)      — multi-step exploration; subagent-friendly.
//     Mirrors upstream's `general` at agent.ts L162-L175.
//
// Built-in Permissions are the *already-merged* cascade (defaults +
// agent-specific deny/allow). For the built-ins we hard-code those mergeds
// so a freshly-constructed Registry is immediately useful in tests; users
// who want to inject a third config layer should construct their own Agent
// with `Permissions: MergePermissions(defaults, userCfg, ownRules)`.
func NewRegistry() *Registry {
	r := &Registry{agents: make(map[string]*Agent, 4)}
	for _, a := range builtinAgents() {
		r.agents[a.Name] = a
	}
	return r
}

// builtinAgents returns the three default agents. Kept as a function (not
// a package-level var) so each NewRegistry call gets a fresh copy — Rule
// slices being mutable, sharing them across registries would let one
// registry's Register mutate another's defaults.
func builtinAgents() []*Agent {
	return []*Agent{
		{
			Name:   "build",
			Mode:   ModePrimary,
			Model:  "anthropic/claude-3-5-sonnet",
			System: "You are the build agent. Execute tools to fulfill user requests.",
			// Wide-open: a single global allow rule. The s10 evaluator
			// will read this as last-match-wins; with only one rule the
			// effect is "everything allowed."
			Permissions: []Rule{
				{Permission: "*", Pattern: "*", Action: ActionAllow},
			},
			// Empty Tools = "all available" — see the Agent doc comment.
			Tools: nil,
		},
		{
			Name:   "plan",
			Mode:   ModePrimary,
			Model:  "anthropic/claude-3-5-sonnet",
			System: "You are the plan agent. Produce a written plan; do not modify the workspace.",
			// Read-only: read/grep/glob allowed; edit/bash/write denied.
			// "ask" is the implicit default for anything else (s04 / s10).
			Permissions: []Rule{
				{Permission: "read", Pattern: "*", Action: ActionAllow},
				{Permission: "grep", Pattern: "*", Action: ActionAllow},
				{Permission: "glob", Pattern: "*", Action: ActionAllow},
				{Permission: "edit", Pattern: "*", Action: ActionDeny},
				{Permission: "write", Pattern: "*", Action: ActionDeny},
				{Permission: "bash", Pattern: "*", Action: ActionDeny},
			},
			// Tools whitelist mirrors the Permissions allow set; s10
			// will refuse to even surface bash/edit/write to the LLM.
			Tools: []string{"read", "grep", "glob"},
		},
		{
			Name:   "general",
			Mode:   ModeAll,
			Model:  "anthropic/claude-3-5-haiku",
			System: "You are the general-purpose subagent. Investigate complex questions across many files.",
			// Multi-step exploration: like build but explicitly denies
			// the high-blast-radius tools so a parent agent can dispatch
			// a "look around" subtask without giving away write access.
			Permissions: []Rule{
				{Permission: "*", Pattern: "*", Action: ActionAllow},
				{Permission: "edit", Pattern: "*", Action: ActionAsk},
				{Permission: "write", Pattern: "*", Action: ActionAsk},
				{Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
			},
			Tools: nil,
		},
	}
}

// Register installs an Agent in the registry, OVERRIDING any built-in (or
// previously-registered) entry with the same name. Returns an error only if
// a is nil or has an empty Name — the registry doesn't otherwise validate
// fields (Permissions and Tools may legitimately be nil).
//
// The "override" semantics mirror upstream's agent.ts L282-L304 loop, which
// looks up an existing entry by key and patches it; if no entry exists, it
// installs a new one. Our simplification: we replace wholesale instead of
// patching field-by-field. That's enough for s09's teaching contract because
// the *Agent constructor at the call site is responsible for filling every
// field they care about (typically by copying from a built-in then mutating).
func (r *Registry) Register(a *Agent) error {
	if a == nil {
		return errors.New("agent: register nil")
	}
	if a.Name == "" {
		return errors.New("agent: register requires a non-empty Name")
	}
	r.agents[a.Name] = a
	return nil
}

// Get returns the agent registered under name. The bool follows the
// `comma, ok` Go idiom — false ⇒ unknown agent, no Agent value. Tests
// pin both built-in lookups (build/plan/general) and the post-Register
// override case.
func (r *Registry) Get(name string) (*Agent, bool) {
	a, ok := r.agents[name]
	return a, ok
}

// ListByMode returns every Agent whose Mode equals m, sorted by Name for
// deterministic output. ModeAll agents are returned ONLY when m == ModeAll
// — they don't auto-promote into ModePrimary or ModeSubagent listings,
// which keeps the contract symmetric ("ask for primary, get exactly the
// primaries"). If your CLI wants the union, call ListByMode three times
// and concatenate, or filter the underlying map yourself.
func (r *Registry) ListByMode(m Mode) []*Agent {
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		if a.Mode == m {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MergePermissions concatenates the three cascade layers in argument order:
//
//	defaults  → userConfig  → agentOverride
//
// (returned slice = defaults ++ userConfig ++ agentOverride)
//
// The contract: the returned slice is suitable for a last-match-wins
// evaluator (the s04 Evaluate semantic — `Array.findLast` upstream at
// `packages/opencode/src/permission/evaluate.ts#L9`). Because later rules
// win, the agent's own rules — which appear LAST — always have the final
// say, exactly the cascade upstream's `Permission.merge(defaults, ..., user)`
// at agent.ts L128 / L143 / L165 produces.
//
// Why a flat concat rather than a smarter merge? Because the evaluator
// itself does the work: it walks the slice in order and remembers the last
// match. A "deduping merge" would have to know about glob equivalence
// (`*.go` vs `*.go` literal vs `*` covering `*.go`) — work that the
// evaluator already does at lookup time. Concatenation keeps both layers
// simple: this fn is structural, the evaluator is semantic.
//
// Nil input slices are treated as empty. The returned slice is freshly
// allocated; callers may mutate it without aliasing the inputs.
func MergePermissions(defaults, userConfig, agentOverride []Rule) []Rule {
	total := len(defaults) + len(userConfig) + len(agentOverride)
	if total == 0 {
		return nil
	}
	out := make([]Rule, 0, total)
	out = append(out, defaults...)
	out = append(out, userConfig...)
	out = append(out, agentOverride...)
	return out
}

// describe is a small helper used only by main's demo to render a one-line
// summary of an Agent. Kept here (not in main.go) so the README/teaching
// notes can point at one stable function the reader can grep.
func (a *Agent) describe() string {
	tools := "all"
	if len(a.Tools) > 0 {
		tools = fmt.Sprintf("%v", a.Tools)
	}
	return fmt.Sprintf("%-8s mode=%s model=%s permissions=%d tools=%s",
		a.Name, a.Mode, a.Model, len(a.Permissions), tools)
}
