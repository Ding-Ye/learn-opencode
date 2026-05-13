package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillFile is a tiny helper used by every test that needs a
// SKILL.md on disk. Returns the file's absolute path so tests can
// assert against Skill.Path.
func writeSkillFile(t *testing.T, root, skillDir, body string) string {
	t.Helper()
	dir := filepath.Join(root, skillDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// TestParseSkillMDValid pins the most basic ParseSkillMD promise:
// given a SKILL.md with valid YAML frontmatter, every field on the
// returned Skill is populated from the right source — name /
// description / when_to_use from frontmatter, body from the content
// after the closing `---`, and Path from the argument.
//
// We deliberately use ParseSkillMD directly (not via DiscoverSkills)
// so a parse-side regression doesn't get diagnosed as a discovery bug.
func TestParseSkillMDValid(t *testing.T) {
	body := `---
name: git-helper
description: Stage, commit, and push changes.
when_to_use: User mentions git or version control.
---

# git-helper body

These are the instructions the model reads when it picks the skill.
`
	s, err := ParseSkillMD("/fake/path/SKILL.md", []byte(body))
	if err != nil {
		t.Fatalf("ParseSkillMD: %v", err)
	}
	if s.Name != "git-helper" {
		t.Errorf("Name = %q, want git-helper", s.Name)
	}
	if s.Description != "Stage, commit, and push changes." {
		t.Errorf("Description = %q, want 'Stage, commit, and push changes.'", s.Description)
	}
	if s.WhenToUse != "User mentions git or version control." {
		t.Errorf("WhenToUse = %q, want 'User mentions git or version control.'", s.WhenToUse)
	}
	if s.Path != "/fake/path/SKILL.md" {
		t.Errorf("Path = %q, want /fake/path/SKILL.md", s.Path)
	}
	if !strings.HasPrefix(s.Body, "# git-helper body") {
		t.Errorf("Body = %q, want it to start with '# git-helper body'", s.Body)
	}
	// Body should NOT contain the frontmatter delimiter or the YAML
	// fields — those are split out into the struct fields.
	if strings.Contains(s.Body, "name: git-helper") {
		t.Errorf("Body still contains frontmatter — split is broken")
	}
}

// TestDiscoverSkillsOneLevelDeep pins the discovery contract:
// DiscoverSkills walks <root>/<skill-name>/SKILL.md (one level deep
// only). It must find every direct sub-dir's SKILL.md; it must NOT
// find a SKILL.md sitting at <root>/SKILL.md (no zero-level), and it
// must NOT recurse into <root>/<a>/<b>/SKILL.md (no two-level).
//
// This is the structural promise that lets readers grep for "where do
// my skills live?" — exactly one nesting level under each configured
// skills root.
func TestDiscoverSkillsOneLevelDeep(t *testing.T) {
	root := t.TempDir()

	// One-level: should be discovered.
	writeSkillFile(t, root, "git-helper", `---
name: git-helper
description: Stage and commit.
---
body`)
	writeSkillFile(t, root, "code-search", `---
name: code-search
description: Search the workspace.
---
body`)

	// Zero-level (root/SKILL.md): should NOT be discovered.
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two-level (root/<a>/<b>/SKILL.md): should NOT be discovered.
	deep := filepath.Join(root, "outer", "inner")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "SKILL.md"), []byte(`---
name: deep-skill
description: should not show up
---
`), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := DiscoverSkills([]string{root})
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("len(skills) = %d, want 2 (only one-level entries)", len(skills))
	}

	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}
	if !names["git-helper"] || !names["code-search"] {
		t.Errorf("missing expected skill names; got %v", names)
	}
	if names["deep-skill"] {
		t.Errorf("deep-skill leaked in — discovery is recursing past one level")
	}
}

// TestDiscoverSkillsLastWinsOnDuplicateName pins the cross-dir
// override contract: when two roots in `dirs` ship a SKILL.md whose
// frontmatter `name` collides, the LATER root in the slice wins —
// matching upstream's "last source overrides earlier source" behavior
// (skill/index.ts L116-L122 plus the L173-L191 scan order).
//
// Why test this explicitly: it's the only place where dir order matters
// to a caller. A future refactor that switches to a map iteration could
// silently swap the winner; this test pins the load-bearing semantic.
func TestDiscoverSkillsLastWinsOnDuplicateName(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	writeSkillFile(t, rootA, "git-helper", `---
name: git-helper
description: from-rootA
when_to_use: triggerA
---
bodyA`)
	writeSkillFile(t, rootB, "git-helper", `---
name: git-helper
description: from-rootB
when_to_use: triggerB
---
bodyB`)

	// rootB is LATER in the slice, so its git-helper should win.
	skills, err := DiscoverSkills([]string{rootA, rootB})
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("len(skills) = %d, want 1 (duplicate names dedup)", len(skills))
	}
	got := skills[0]
	if got.Description != "from-rootB" {
		t.Errorf("Description = %q, want from-rootB (last wins)", got.Description)
	}
	if !strings.HasPrefix(got.Body, "bodyB") {
		t.Errorf("Body = %q, want it to start with bodyB", got.Body)
	}

	// Reverse the order: now rootA should win.
	skills2, err := DiscoverSkills([]string{rootB, rootA})
	if err != nil {
		t.Fatalf("DiscoverSkills (reversed): %v", err)
	}
	if len(skills2) != 1 {
		t.Fatalf("len(skills2) = %d, want 1", len(skills2))
	}
	if skills2[0].Description != "from-rootA" {
		t.Errorf("after reverse: Description = %q, want from-rootA", skills2[0].Description)
	}
}

// TestParseSkillMDMissingFrontmatter pins the fail-loud contract: a
// SKILL.md without leading `---` is NOT a "skill with empty fields" —
// it's an error. Same for one with an opening `---` but no closing
// `---`. The error message must mention the file path so a user with
// 50 skills knows which one is broken.
func TestParseSkillMDMissingFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // substring that MUST appear in the error
	}{
		{
			name:    "no leading delimiter",
			content: "# Just a markdown file\n\nNo frontmatter here.\n",
			want:    "missing leading",
		},
		{
			name:    "leading but no closing delimiter",
			content: "---\nname: orphan\ndescription: no close\n\n# body never marked as such\n",
			want:    "missing closing",
		},
		{
			name: "missing required name field",
			content: `---
description: I forgot the name
---
body
`,
			want: "missing required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "BAD.md")
			_, err := ParseSkillMD(path, []byte(tc.content))
			if err == nil {
				t.Fatalf("ParseSkillMD returned nil error; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want it to contain the path %q", err.Error(), path)
			}
		})
	}
}

// TestCatalogStringFormat pins the system-prompt-ready format. Each
// skill renders as:
//
//	- <name>: <description> (use when: <when_to_use>)
//
// Skills are joined with "\n" between entries (not after the last).
// Empty WhenToUse → omit the "(use when: …)" suffix entirely.
//
// We also assert all three frontmatter fields show up in the output —
// this is the "the catalog actually surfaces what it discovered"
// contract that drives s10's system-prompt assembly.
func TestCatalogStringFormat(t *testing.T) {
	skills := []*Skill{
		{
			Name:        "git-helper",
			Description: "Stage and commit changes.",
			WhenToUse:   "user mentions git",
		},
		{
			Name:        "no-trigger",
			Description: "A skill without a when_to_use field.",
			WhenToUse:   "",
		},
	}

	got := CatalogString(skills)

	// Required substrings — all three fields must surface.
	mustContain := []string{
		"git-helper",
		"Stage and commit changes.",
		"use when: user mentions git",
		"no-trigger",
		"A skill without a when_to_use field.",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("CatalogString missing %q; got:\n%s", want, got)
		}
	}

	// Exact line for the first skill, so a future refactor can't silently
	// change the format.
	wantLine := "- git-helper: Stage and commit changes. (use when: user mentions git)"
	if !strings.Contains(got, wantLine) {
		t.Errorf("expected exact line %q, got:\n%s", wantLine, got)
	}

	// Empty WhenToUse → no "(use when:" for that skill. (We assert by
	// counting occurrences: should be exactly 1 — git-helper's.)
	if got, want := strings.Count(got, "(use when:"), 1; got != want {
		t.Errorf("'(use when:' count = %d, want %d (only git-helper has WhenToUse)", got, want)
	}

	// Empty input → empty string (so callers can `if cat != "" { ... }`).
	if got := CatalogString(nil); got != "" {
		t.Errorf("CatalogString(nil) = %q, want empty string", got)
	}
	if got := CatalogString([]*Skill{}); got != "" {
		t.Errorf("CatalogString([]) = %q, want empty string", got)
	}
}
