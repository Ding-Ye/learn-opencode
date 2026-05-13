package main

import "fmt"

// main is a hand-runnable demo of the permission evaluator at work:
//
//	go run .
//
// Builds a three-layer ruleset cascade — built-in defaults, a user-config
// override, and an agent-specific override — then queries it for a few
// realistic (permission, target) pairs and prints the verdict. This is
// exactly the call shape s09 (agent-registry) will use when a tool dispatch
// needs a permission verdict before invoking the tool.
func main() {
	// Layer 1: built-in defaults — ask for any edit, deny `rm -rf` shells.
	defaults := Ruleset{
		{Permission: "edit", Pattern: "*", Action: ActionAsk},
		{Permission: "bash", Pattern: "rm -rf *", Action: ActionDeny},
	}

	// Layer 2: user opencode.json — trust .go edits, allow git commands.
	userConfig := Ruleset{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow},
		{Permission: "bash", Pattern: "git *", Action: ActionAllow},
	}

	// Layer 3: agent override (e.g. the `plan` agent) — never edit secrets.
	agentOverride := Ruleset{
		{Permission: "edit", Pattern: "secrets.go", Action: ActionDeny},
	}

	probes := []struct {
		permission, target string
	}{
		{"edit", "main.go"},      // user allow wins over default ask
		{"edit", "secrets.go"},   // agent deny wins over user allow
		{"edit", "README.md"},    // only default `*` matches → ask
		{"bash", "git status"},   // user allow `git *` wins
		{"bash", "rm -rf /"},     // default deny `rm -rf *` wins
		{"bash", "echo hi"},      // no match → ask
		{"webfetch", "anything"}, // permission has no rules anywhere → ask
	}

	fmt.Println("--- permission cascade demo (defaults → userConfig → agentOverride) ---")
	for _, p := range probes {
		verdict := Evaluate(p.permission, p.target, defaults, userConfig, agentOverride)
		fmt.Printf("  %-10s %-20s → %s\n", p.permission, p.target, verdict)
	}
}
