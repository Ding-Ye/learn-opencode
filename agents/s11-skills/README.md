# s11 — skills

s08 loaded the user's `opencode.json`. s10 wired the streaming tool
loop. s11 plugs in the missing piece between them: **skills**, the
markdown files a user drops into `.opencode/skills/<name>/SKILL.md` (or
`~/.opencode/skills/...`) so the model knows "use the git-helper
skill when the user mentions git." Each SKILL.md has YAML frontmatter
declaring `name`, `description`, and (our extension) `when_to_use`.
s11 walks the configured roots, parses every frontmatter, and renders
a **skill catalog string** that s10 can prepend to the system prompt.

The mechanism mirrors `packages/opencode/src/skill/index.ts`:
discovery + frontmatter parse + catalog formatting. Three pieces, ~350
LOC including tests.

## Files

- `skill.go` — three exported functions and the `Skill` struct:
  - `type Skill struct { Name, Description, WhenToUse, Body, Path string }`
  - `ParseSkillMD(path, content) (*Skill, error)` — splits the
    `---`-delimited frontmatter from the markdown body and yaml-
    unmarshals the frontmatter. Missing leading `---`, missing closing
    `---`, or missing `name` field → error with the file path baked in.
  - `DiscoverSkills(dirs []string) ([]*Skill, error)` — walks each dir
    looking for `<dir>/<skill-name>/SKILL.md` (one level deep — mirrors
    upstream's `EXTERNAL_SKILL_PATTERN = "skills/**/SKILL.md"`
    restricted to depth 1 for teaching). Last-wins on duplicate `name`
    across dirs; preserves the order skills were first seen.
  - `CatalogString(skills []*Skill) string` — renders one line per
    skill: `- <name>: <description> (use when: <when_to_use>)`. Empty
    `WhenToUse` → suffix omitted. Empty input → empty string (so the
    caller can gate the whole "Available Skills:" prompt section on
    `cat != ""`).
- `main.go` — short demo. Writes two SKILL.md files into a temp dir,
  calls `DiscoverSkills` + `CatalogString`, prints both. Cleans up the
  temp dir on exit. No network.
- `skill_test.go` — 5 tests, all `t.TempDir()`-based:
  1. **ParseSkillMDValid** — every field on `Skill` populated from the
     right source (frontmatter / body / Path arg).
  2. **DiscoverSkillsOneLevelDeep** — finds `<root>/<skill>/SKILL.md`;
     ignores `<root>/SKILL.md` (zero-level) and `<root>/<a>/<b>/SKILL.md`
     (two-level).
  3. **DiscoverSkillsLastWinsOnDuplicateName** — when two roots ship a
     skill with the same `name`, the LATER root in `dirs` wins.
  4. **ParseSkillMDMissingFrontmatter** — three sub-cases (no leading
     `---`, no closing `---`, no `name` field). Each errors loudly with
     the path in the message.
  5. **CatalogStringFormat** — pins the exact output line format and
     all three frontmatter fields surface; empty WhenToUse omits suffix;
     empty input returns empty string.

## Run

```bash
# Demo (deterministic, no network)
go run .

# 5 tests
go test -count=1 ./...

# Vet + build + test in one go
go vet ./... && go build ./... && go test -count=1 ./...
```

## Key teaching points

- **Three layers, one file**. Frontmatter declares (name, description,
  when_to_use); the body is everything after the closing `---`. The
  parser doesn't validate the body — it's the model's territory.
- **Last-wins on duplicates**. Mirrors upstream's L116-L122
  "warn-and-overwrite" loop. Caller decides priority by `dirs` order:
  put global dirs first, project dirs last (or vice versa, your call).
- **Catalog is structural**. `CatalogString` is pure formatting — no
  ordering, no filtering by agent permissions. s10 (or a future
  caller) does the cascade work; s11 just renders what was discovered.
- **Body is loaded eagerly but used lazily**. The full markdown body
  goes into `Skill.Body` so the demo can show it, but in production a
  loop would only emit the catalog (name + description + trigger) into
  the system prompt and let the model fetch the body via a
  hypothetical `read_skill` tool when it picks the skill. We carry it
  to keep the struct one-shot useful, not because the catalog needs it.

See `docs/zh/s11-skills.md` and `docs/en/s11-skills.md` for the
long-form walkthrough plus the upstream `skill/index.ts` excerpt that
motivates the discovery + last-wins + catalog rendering pipeline.
