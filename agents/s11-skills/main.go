package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// main is a hand-runnable demo of the skill registry:
//
//	go run .
//
// It does four things, in order:
//
//  1. Build a fresh temp dir and write two SKILL.md files (git-helper
//     and code-search) into it, mirroring the on-disk layout opencode
//     looks for: <root>/<skill-name>/SKILL.md.
//  2. Call DiscoverSkills to walk the temp dir.
//  3. Print each discovered skill's (name, description, when_to_use,
//     path) so you can see exactly what came off disk.
//  4. Call CatalogString on the result and print the "system-prompt-
//     ready" string — that's what s10's loop would inject into the
//     LLM's system prompt.
//
// Deterministic, no network, no env vars touched. Cleans up its temp
// dir on exit.
func main() {
	root, err := os.MkdirTemp("", "s11-skills-demo-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir temp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(root)

	skills := []struct {
		dir  string
		body string
	}{
		{
			dir: "git-helper",
			body: `---
name: git-helper
description: Stage, commit, and push changes with conventional commit messages.
when_to_use: User mentions git, staging, committing, or pushing.
---

# git-helper

Detailed instructions for the model on how to use git…
`,
		},
		{
			dir: "code-search",
			body: `---
name: code-search
description: Search the workspace with ripgrep and surface matching files.
when_to_use: User wants to grep, find a string, or locate a file.
---

# code-search

Detailed instructions on running rg…
`,
		},
	}
	for _, s := range skills {
		dir := filepath.Join(root, s.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(s.body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write skill:", err)
			os.Exit(1)
		}
	}

	discovered, err := DiscoverSkills([]string{root})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stdout, "discovered skills:")
	for _, s := range discovered {
		fmt.Fprintf(os.Stdout, "  - name=%s\n", s.Name)
		fmt.Fprintf(os.Stdout, "    description=%s\n", s.Description)
		fmt.Fprintf(os.Stdout, "    when_to_use=%s\n", s.WhenToUse)
		fmt.Fprintf(os.Stdout, "    path=%s\n", s.Path)
	}

	fmt.Fprintln(os.Stdout, "\ncatalog string (what s10 would inject into the system prompt):")
	fmt.Fprintln(os.Stdout, CatalogString(discovered))
	fmt.Fprintln(os.Stdout, "\n(s10's loop would prepend this to the agent's system prompt before each request.)")
}
