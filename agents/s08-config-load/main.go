package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// main is a hand-runnable demo of hierarchical config loading:
//
//	go run .
//
// Builds two temp directories — a fake home with `~/.opencode/opencode.json`
// and a fake project tree with `<cwd>/.opencode/opencode.json` — calls Load,
// and prints the merged Config as indented JSON. Deterministic, no network,
// no env vars set in the process (the env override is exercised by passing
// an explicit map).
//
// The demo's job is to make the *order* obvious: project overrides user,
// env overrides both, and Instructions[] concatenates instead of replacing.
// Read the printed Config alongside the two source files (the demo prints
// each before merging) and the precedence is one trace through.
func main() {
	tmp, err := os.MkdirTemp("", "s08-demo-")
	if err != nil {
		log.Fatalf("mkdir tmp: %v", err)
	}
	defer os.RemoveAll(tmp)

	// Fake home with a global opencode.json.
	homeDir := filepath.Join(tmp, "home")
	if err := writeJSON(filepath.Join(homeDir, ".opencode", "opencode.json"), map[string]any{
		"provider":     map[string]string{"provider": "anthropic", "model": "claude-3-haiku"},
		"instructions": []string{"~/CLAUDE.md", "~/global-style.md"},
		"permissions": []map[string]string{
			{"permission": "edit", "pattern": "*", "action": "ask"},
			{"permission": "bash", "pattern": "rm -rf*", "action": "deny"},
		},
		"skills": map[string]string{"global": "~/.opencode/skills"},
	}); err != nil {
		log.Fatalf("write home config: %v", err)
	}

	// Fake project at <tmp>/proj/sub/here — config lives at proj/.opencode/.
	cwd := filepath.Join(tmp, "proj", "sub", "here")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		log.Fatalf("mkdir cwd: %v", err)
	}
	if err := writeJSON(filepath.Join(tmp, "proj", ".opencode", "opencode.json"), map[string]any{
		// Override the model, but leave provider unchanged.
		"provider": map[string]string{"model": "claude-3-5-sonnet"},
		// Project-side instructions concat onto user-side.
		"instructions": []string{"AGENTS.md", "docs/style.md"},
		// Project-side permission appended (and replaces user-side per the
		// "override-wins-if-non-empty" rule for slices that aren't Instructions).
		"permissions": []map[string]string{
			{"permission": "edit", "pattern": "*.go", "action": "allow"},
		},
	}); err != nil {
		log.Fatalf("write project config: %v", err)
	}

	// Pretend `OPENCODE_MODEL=claude-3-opus` was set — overrides both files.
	env := map[string]string{"OPENCODE_MODEL": "claude-3-opus"}

	cfg, err := Load(cwd, homeDir, env)
	if err != nil {
		log.Fatalf("Load: %v", err)
	}

	pretty, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Fprintln(os.Stdout, "merged config:")
	fmt.Fprintln(os.Stdout, string(pretty))
}

// writeJSON is a one-liner so main stays focused on the precedence story.
// Creates the parent dir, marshals indented, writes 0644.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
