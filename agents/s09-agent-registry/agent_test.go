package main

import "testing"

// TestBuiltinAgentsResolve pins the most basic Registry promise: a newly-
// constructed Registry returns the three named built-ins with reasonable
// defaults. If this fails, every later test (and the demo) is meaningless
// — there's no Registry to register against.
//
// We also assert each built-in's Mode, because mode is what ListByMode
// filters on; a built-in tagged with the wrong mode would silently
// disappear from a "list primary agents" CLI command.
func TestBuiltinAgentsResolve(t *testing.T) {
	r := NewRegistry()

	cases := []struct {
		name string
		mode Mode
	}{
		{"build", ModePrimary},
		{"plan", ModePrimary},
		{"general", ModeAll},
	}
	for _, tc := range cases {
		a, ok := r.Get(tc.name)
		if !ok {
			t.Fatalf("Get(%q) missing — built-ins must be present after NewRegistry", tc.name)
		}
		if a.Name != tc.name {
			t.Errorf("Get(%q).Name = %q, want %q", tc.name, a.Name, tc.name)
		}
		if a.Mode != tc.mode {
			t.Errorf("Get(%q).Mode = %v, want %v", tc.name, a.Mode, tc.mode)
		}
		if len(a.Permissions) == 0 {
			t.Errorf("Get(%q).Permissions empty — built-ins must ship a usable ruleset", tc.name)
		}
	}

	// And an unknown name returns ok=false so callers can distinguish
	// "agent not configured" from "agent configured with empty fields."
	if _, ok := r.Get("nonexistent"); ok {
		t.Errorf("Get('nonexistent') unexpectedly returned ok=true")
	}
}

// TestUserDefinedAgentOverridesBuiltin pins the load-bearing semantics
// behind opencode's `cfg.agent` override loop (agent.ts L282-L304): when
// a user declares an agent with the same name as a built-in, the user's
// version wins entirely — same model, same permissions, no inheritance.
//
// Why total override (not patch-merge): patch-merge requires picking
// per-field defaults; total override forces the caller to be explicit
// about every field they care about. The ergonomic tax is paid once at
// the call site (copy from a built-in, then mutate); the semantic
// simplicity benefits everything downstream that has to reason about
// "what's the model for agent X?"
func TestUserDefinedAgentOverridesBuiltin(t *testing.T) {
	r := NewRegistry()

	original, _ := r.Get("build")
	originalModel := original.Model

	custom := &Agent{
		Name:        "build",
		Mode:        ModePrimary,
		Model:       "openai/gpt-4o",
		System:      "custom build prompt",
		Permissions: []Rule{{Permission: "*", Pattern: "*", Action: ActionAsk}},
	}
	if err := r.Register(custom); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := r.Get("build")
	if !ok {
		t.Fatalf("Get('build') vanished after Register")
	}
	if got.Model == originalModel {
		t.Errorf("Get('build').Model = %q (the built-in); register should override", got.Model)
	}
	if got.Model != "openai/gpt-4o" {
		t.Errorf("Get('build').Model = %q, want openai/gpt-4o", got.Model)
	}
	if got.System != "custom build prompt" {
		t.Errorf("Get('build').System = %q, want custom build prompt", got.System)
	}

	// Register on a fresh name installs alongside the built-ins, doesn't
	// disturb them.
	r2 := NewRegistry()
	if err := r2.Register(&Agent{Name: "researcher", Mode: ModeSubagent}); err != nil {
		t.Fatalf("Register(researcher): %v", err)
	}
	if _, ok := r2.Get("researcher"); !ok {
		t.Errorf("Get('researcher') missing after Register")
	}
	if _, ok := r2.Get("build"); !ok {
		t.Errorf("Get('build') vanished after registering an unrelated agent")
	}

	// And the validation contract: nil / empty-name → error.
	if err := r2.Register(nil); err == nil {
		t.Errorf("Register(nil) should error")
	}
	if err := r2.Register(&Agent{Name: ""}); err == nil {
		t.Errorf("Register(empty name) should error")
	}
}

// TestMergePermissionsConcatenatesInOrder pins the cascade contract: the
// returned slice is exactly defaults ++ userConfig ++ agentOverride, in
// that order, byte-for-byte. The evaluator (s04 / s10) reads it as
// last-match-wins, so this order = "agent has the final say."
//
// Why this test exists: a tempting refactor would be to dedupe rules
// with the same (Permission, Pattern) pair, keeping only the last —
// the result LOOKS the same to a last-match-wins evaluator. We don't
// dedupe, because (a) the evaluator already does the work and (b)
// dedup would erase the cascade audit trail (you couldn't see that
// "the agent's own deny overrode the user's allow" by reading the
// merged slice). Pinning the literal-concat output prevents that
// well-meaning refactor.
func TestMergePermissionsConcatenatesInOrder(t *testing.T) {
	defaults := []Rule{
		{Permission: "*", Pattern: "*", Action: ActionAsk},
		{Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
	}
	userConfig := []Rule{
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow},
	}
	agentOverride := []Rule{
		{Permission: "edit", Pattern: "secrets.go", Action: ActionDeny},
	}

	merged := MergePermissions(defaults, userConfig, agentOverride)

	want := []Rule{
		{Permission: "*", Pattern: "*", Action: ActionAsk},
		{Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
		{Permission: "edit", Pattern: "*.go", Action: ActionAllow},
		{Permission: "edit", Pattern: "secrets.go", Action: ActionDeny},
	}
	if len(merged) != len(want) {
		t.Fatalf("len(merged) = %d, want %d (defaults ++ user ++ agent)", len(merged), len(want))
	}
	for i, w := range want {
		if merged[i] != w {
			t.Errorf("merged[%d] = %+v, want %+v", i, merged[i], w)
		}
	}

	// Empty layers contribute nothing; their absence doesn't shift others.
	merged2 := MergePermissions(nil, userConfig, nil)
	if len(merged2) != 1 || merged2[0] != userConfig[0] {
		t.Errorf("merged2 = %+v, want exactly the userConfig slice", merged2)
	}

	// All-empty input → nil result (so callers can ergonomically
	// `if len(rules) == 0 { return ActionAsk }` without an extra branch).
	if got := MergePermissions(nil, nil, nil); got != nil {
		t.Errorf("MergePermissions(nil, nil, nil) = %v, want nil", got)
	}

	// And the returned slice is independent — mutating it doesn't change
	// the input slices. (Important: callers in s10 may filter or append.)
	merged3 := MergePermissions(defaults, nil, nil)
	merged3[0].Action = ActionDeny
	if defaults[0].Action != ActionAsk {
		t.Errorf("defaults[0] aliased — MergePermissions returned a non-independent slice")
	}
}

// TestListByModeReturnsOnlyMatching pins the filter contract for ListByMode:
// it returns exactly the agents whose Mode matches m, with no auto-promotion
// (a ModeAll agent does NOT also appear in a ModePrimary listing). Sorted
// by Name so the demo's output and tests are deterministic.
//
// Three asserts: built-in primary set is exactly {build, plan}; built-in
// subagent set is empty (general is ModeAll, not ModeSubagent); a freshly
// registered ModeSubagent agent appears in the subagent listing.
func TestListByModeReturnsOnlyMatching(t *testing.T) {
	r := NewRegistry()

	primary := r.ListByMode(ModePrimary)
	if len(primary) != 2 {
		t.Fatalf("primary count = %d, want 2 (build, plan)", len(primary))
	}
	// Sorted by Name, so position is fixed.
	if primary[0].Name != "build" || primary[1].Name != "plan" {
		t.Errorf("primary names = [%s %s], want [build plan]", primary[0].Name, primary[1].Name)
	}

	// general is ModeAll — must NOT appear in the subagent list.
	subagents := r.ListByMode(ModeSubagent)
	for _, a := range subagents {
		if a.Name == "general" {
			t.Errorf("general (ModeAll) leaked into ModeSubagent listing")
		}
	}
	if len(subagents) != 0 {
		t.Errorf("subagent count = %d, want 0 (no built-in subagents)", len(subagents))
	}

	// Add a real subagent and confirm it surfaces.
	if err := r.Register(&Agent{Name: "researcher", Mode: ModeSubagent}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	subagents2 := r.ListByMode(ModeSubagent)
	if len(subagents2) != 1 || subagents2[0].Name != "researcher" {
		t.Errorf("subagents after Register = %+v, want one researcher entry", subagents2)
	}

	// And the ModeAll listing surfaces general but not the others.
	all := r.ListByMode(ModeAll)
	if len(all) != 1 || all[0].Name != "general" {
		t.Errorf("ModeAll listing = %+v, want exactly [general]", all)
	}
}
