package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeConfig is the testing fixture: marshals `v` to JSON, writes to
// <baseDir>/.opencode/opencode.json. Used by every test below.
func writeConfig(t *testing.T, baseDir string, v any) {
	t.Helper()
	dir := filepath.Join(baseDir, ".opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestProjectOnlyConfig pins the basic "no user config, just a project
// `.opencode/opencode.json` in cwd" path. This is the single most common
// shape in the wild — a checked-in project config with no user-level
// global. Everything in the file should appear in the returned Config.
//
// Why it matters: the walk-upward code path runs unconditionally; if the
// user-config plumbing had a bug that wrote `nil` into the merged result,
// it would silently zero out the project values. This test catches that.
func TestProjectOnlyConfig(t *testing.T) {
	cwd := t.TempDir()
	writeConfig(t, cwd, map[string]any{
		"provider":     map[string]string{"provider": "anthropic", "model": "claude-3-5-sonnet"},
		"instructions": []string{"AGENTS.md", "docs/style.md"},
		"permissions": []map[string]string{
			{"permission": "edit", "pattern": "*.go", "action": "allow"},
		},
	})

	// Empty homeDir == "no user config" — the test for that mode is
	// separate from the project-only mode pinned here.
	cfg, err := Load(cwd, "", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider.ProviderID != "anthropic" || cfg.Provider.ModelID != "claude-3-5-sonnet" {
		t.Errorf("provider = %+v, want anthropic/claude-3-5-sonnet", cfg.Provider)
	}
	if len(cfg.Instructions) != 2 || cfg.Instructions[0] != "AGENTS.md" {
		t.Errorf("instructions = %v, want [AGENTS.md docs/style.md]", cfg.Instructions)
	}
	if len(cfg.Permissions) != 1 || cfg.Permissions[0].Action != ActionAllow {
		t.Errorf("permissions = %+v, want one allow rule", cfg.Permissions)
	}
}

// TestUserOnlyConfig pins the symmetric case: nothing in the project's
// cwd, only a `<homeDir>/.opencode/opencode.json`. The user config alone
// should populate the returned Config. If the project-walk somehow
// clobbered the user values to zero (a "merge with zero result" bug),
// this test catches it.
func TestUserOnlyConfig(t *testing.T) {
	homeDir := t.TempDir()
	cwd := t.TempDir() // exists but has no .opencode/ in it or above

	writeConfig(t, homeDir, map[string]any{
		"provider":     map[string]string{"provider": "anthropic", "model": "claude-3-haiku"},
		"instructions": []string{"~/CLAUDE.md"},
		"skills":       map[string]string{"global": "~/.opencode/skills"},
	})

	cfg, err := Load(cwd, homeDir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider.ModelID != "claude-3-haiku" {
		t.Errorf("ModelID = %q, want claude-3-haiku", cfg.Provider.ModelID)
	}
	if len(cfg.Instructions) != 1 || cfg.Instructions[0] != "~/CLAUDE.md" {
		t.Errorf("instructions = %v, want [~/CLAUDE.md]", cfg.Instructions)
	}
	if cfg.Skills["global"] != "~/.opencode/skills" {
		t.Errorf("skills[global] = %q, want ~/.opencode/skills", cfg.Skills["global"])
	}
}

// TestProjectOverridesUser pins the precedence rule: when both files
// declare the same scalar field, the project wins. The user config
// sets ModelID to haiku; the project sets it to sonnet; the merged
// result must be sonnet. This is the "you can pin a per-repo model"
// promise that future s09 / s10 will lean on.
//
// We also assert the *user* fields that the project DIDN'T override
// still survive (Skills here). That's the deep-merge property —
// override doesn't replace the whole config, just the fields it touches.
func TestProjectOverridesUser(t *testing.T) {
	homeDir := t.TempDir()
	cwd := t.TempDir()

	writeConfig(t, homeDir, map[string]any{
		"provider": map[string]string{"provider": "anthropic", "model": "claude-3-haiku"},
		"skills":   map[string]string{"global": "~/.opencode/skills"},
	})
	writeConfig(t, cwd, map[string]any{
		// Override model only — leave provider unchanged to verify
		// per-field merge (not whole-struct replacement).
		"provider": map[string]string{"model": "claude-3-5-sonnet"},
	})

	cfg, err := Load(cwd, homeDir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider.ProviderID != "anthropic" {
		t.Errorf("ProviderID = %q, want anthropic (from user, unmodified)", cfg.Provider.ProviderID)
	}
	if cfg.Provider.ModelID != "claude-3-5-sonnet" {
		t.Errorf("ModelID = %q, want claude-3-5-sonnet (project override)", cfg.Provider.ModelID)
	}
	if cfg.Skills["global"] != "~/.opencode/skills" {
		t.Errorf("skills[global] = %q, want ~/.opencode/skills (user, not overridden)", cfg.Skills["global"])
	}
}

// TestInstructionsConcatenated pins the load-bearing array-concat exception.
// Instructions[] is the ONLY field that concats (user-first, project-after)
// instead of override-replacing. The reason: each instruction path is a
// system-prompt fragment, and the user wants their global CLAUDE.md AND
// the per-project AGENTS.md to both appear. Replacement would silently
// drop one or the other.
//
// We also assert dedup: the same path appearing in both files only shows
// up once in the merged result. Mirrors upstream's `new Set([...])` step.
func TestInstructionsConcatenated(t *testing.T) {
	homeDir := t.TempDir()
	cwd := t.TempDir()

	writeConfig(t, homeDir, map[string]any{
		"instructions": []string{"~/CLAUDE.md", "shared.md"},
	})
	writeConfig(t, cwd, map[string]any{
		"instructions": []string{"AGENTS.md", "shared.md"}, // "shared.md" dedup'd
	})

	cfg, err := Load(cwd, homeDir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"~/CLAUDE.md", "shared.md", "AGENTS.md"}
	if len(cfg.Instructions) != len(want) {
		t.Fatalf("instructions = %v, want %v (user first, then project, dedup)", cfg.Instructions, want)
	}
	for i, w := range want {
		if cfg.Instructions[i] != w {
			t.Errorf("instructions[%d] = %q, want %q", i, cfg.Instructions[i], w)
		}
	}
}

// TestEnvOverrideOfProviderModel pins the highest-priority layer: env vars
// win over both files. This is the runtime "do this once different" knob —
// `OPENCODE_MODEL=claude-3-opus go run .` should take effect without
// touching either JSON file.
//
// We set up *both* files declaring a different model so the test would
// fail if env-override ran in the wrong order (e.g. before merge, which
// would let the project re-overwrite it).
func TestEnvOverrideOfProviderModel(t *testing.T) {
	homeDir := t.TempDir()
	cwd := t.TempDir()

	writeConfig(t, homeDir, map[string]any{
		"provider": map[string]string{"provider": "anthropic", "model": "claude-3-haiku"},
	})
	writeConfig(t, cwd, map[string]any{
		"provider": map[string]string{"model": "claude-3-5-sonnet"},
	})

	env := map[string]string{
		"OPENCODE_MODEL":    "claude-3-opus",
		"OPENCODE_PROVIDER": "anthropic",
	}
	cfg, err := Load(cwd, homeDir, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider.ModelID != "claude-3-opus" {
		t.Errorf("ModelID = %q, want claude-3-opus (env wins over both files)", cfg.Provider.ModelID)
	}
	if cfg.Provider.ProviderID != "anthropic" {
		t.Errorf("ProviderID = %q, want anthropic", cfg.Provider.ProviderID)
	}

	// And confirm absence of the env var leaves the file-level merge result
	// intact (so the env-override path is the only thing that bumps it).
	cfg2, err := Load(cwd, homeDir, nil)
	if err != nil {
		t.Fatalf("Load (no env): %v", err)
	}
	if cfg2.Provider.ModelID != "claude-3-5-sonnet" {
		t.Errorf("ModelID (no env) = %q, want claude-3-5-sonnet (project beats user)", cfg2.Provider.ModelID)
	}
}
