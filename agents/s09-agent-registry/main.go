package main

import (
	"fmt"
	"os"
)

// main is a hand-runnable demo of the agent registry:
//
//	go run .
//
// It does four things, in order:
//
//  1. Build a fresh Registry (which seeds the three built-ins).
//  2. List every primary agent (build + plan, sorted by name) — the set
//     a CLI would offer at session start.
//  3. Override "plan" with a custom system prompt, demonstrating the
//     Register-overrides-built-in semantics.
//  4. Construct a three-layer permission cascade for the "build" agent,
//     run MergePermissions over the layers, and print the resulting slice
//     in concat order so the cascade contract is visible at a glance.
//
// Deterministic, no network, no env vars touched.
func main() {
	r := NewRegistry()

	fmt.Fprintln(os.Stdout, "primary agents (built-in):")
	for _, a := range r.ListByMode(ModePrimary) {
		fmt.Fprintln(os.Stdout, " ", a.describe())
	}

	// Override "plan" — same name, different system prompt + model. After
	// Register, Get("plan") returns the override; the original built-in is
	// gone. (The order-of-operations matters for s09's contract: a user's
	// `cfg.agent.plan = {...}` always wins over the built-in.)
	override := &Agent{
		Name:        "plan",
		Mode:        ModePrimary,
		Model:       "anthropic/claude-3-opus",
		System:      "You are a strict planner. Output a numbered list, nothing else.",
		Permissions: []Rule{{Permission: "*", Pattern: "*", Action: ActionAsk}},
		Tools:       []string{"read"},
	}
	if err := r.Register(override); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "\nafter overriding plan:")
	if a, ok := r.Get("plan"); ok {
		fmt.Fprintln(os.Stdout, " ", a.describe())
	}

	// Three-layer cascade for the "build" agent. In production, `defaults`
	// comes from a hard-coded ruleset (the safe baseline), `userConfig`
	// comes from `cfg.Permissions` loaded by s08, and `agentOverride`
	// comes from the agent's own `permissions[]` block in opencode.json.
	// MergePermissions concatenates them in argument order; the evaluator
	// (s04's last-match-wins) reads the result.
	defaults := []Rule{
		{Permission: "edit", Pattern: "*", Action: ActionAsk},
		{Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
	}
	userConfig := []Rule{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow},
	}
	build, _ := r.Get("build")
	merged := MergePermissions(defaults, userConfig, build.Permissions)

	fmt.Fprintln(os.Stdout, "\ncascade for 'build' (defaults ++ user ++ agent):")
	for i, rule := range merged {
		fmt.Fprintf(os.Stdout, "  [%d] %-6s %-12s -> %s\n",
			i, rule.Permission, rule.Pattern, rule.Action)
	}
	fmt.Fprintln(os.Stdout, "(s10's evaluator walks this slice and remembers the LAST match.)")
}
