---
title: "s11 · Skill discovery"
chapter: 11
slug: s11-skills
est_read_min: 10
---

# s11 · Skill discovery

> What this chapter teaches: walk one or more skill roots on disk, find every `SKILL.md` at exactly one nesting level, parse its YAML frontmatter into a `(name, description, when_to_use, body)` tuple, dedup by name with **last-wins**, and render a system-prompt-ready catalog string. The mechanism that lets a user drop `mkdir -p .opencode/skills/git-helper && $EDITOR SKILL.md` and have the next session's LLM know "use the git-helper skill when the user mentions git" — no code change, no restart, no config file edit beyond the SKILL.md itself.

---

## Problem

Through s10 the agent has a working tool loop, but every prompt is just (system prompt) + (user message) + (tool catalog). Users want a third thing: **skills** — a `SKILL.md` file the user drops on disk to say "here's a recipe; use it when X." The skill's frontmatter advertises (name, description, trigger hint); the body is the recipe the model reads when it picks the skill.

Concrete pains the agent without skills hits:

- A user has a personal "commit with conventional-commits prefix and emoji" recipe. They DON'T want to bake it into the system prompt of every chat — they want the model to USE the recipe only when it's actually committing.
- The same project has a "code-search uses ripgrep with `--type-not vendor`" idiom, and a "deployment goes through the make-deploy script" idiom. Two separate skills, two different triggers, both should be available for the model to choose from.
- A user has a global skill in `~/.opencode/skills/git-helper/SKILL.md` AND a project-specific override in `.opencode/skills/git-helper/SKILL.md`. The project override should win — the user expects the more-specific version to dominate.

opencode's answer is **discover SKILL.md files from a list of skill roots, parse each one's frontmatter, dedup by name with last-wins, and inject the resulting catalog into the system prompt**. The body of each SKILL.md isn't read by every request — it's there for when the model picks the skill (a hypothetical `read_skill` tool the LLM invokes).

s11 builds the *discovery + parse + catalog* pipeline. It does NOT build:

- The system-prompt assembler that concatenates `(agent system) + (skill catalog) + ...` (s10's job, extended in a later session).
- The `read_skill` tool that lets the model fetch a body once it picks the skill (out of scope; a 30-line wrapper around `os.ReadFile`).
- The git-pull-from-URL `cfg.skills.urls` discovery flavor (out of scope; would require a git client).
- The "external dirs" discovery (`.claude/`, `.agents/`) — same mechanism, just different roots; cfg-paths plus s11 covers it.

## Solution

A struct, three functions, and one parser:

```go
type Skill struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description"`
    WhenToUse   string `yaml:"when_to_use"`
    Body        string `yaml:"-"`
    Path        string `yaml:"-"`
}

func ParseSkillMD(path string, content []byte) (*Skill, error)
func DiscoverSkills(dirs []string) ([]*Skill, error)
func CatalogString(skills []*Skill) string
```

What each does:

- **`ParseSkillMD`**: split content at the first `---` (must be line 1) and the next `---` line. Yaml-unmarshal the frontmatter into the struct's three exported tag-bearing fields. Body is everything after the closing `---` (with leading blank lines trimmed). Missing leading delimiter, missing closing delimiter, and missing required `name` are all hard errors with the file path baked into the message.
- **`DiscoverSkills`**: for each directory in `dirs`, list its DIRECT sub-directories (one level deep), and check each for `SKILL.md`. Parse every found SKILL.md. Dedup by `name` with **last-wins** (later `dirs` entries overwrite earlier ones). Missing root dirs are silently skipped (mirrors upstream's `if (!isDir(root)) continue`); a parse error on any file fails the whole call (teaching contract).
- **`CatalogString`**: render one line per skill in the input slice's order. Format: `- <name>: <description> (use when: <when_to_use>)`. Empty `WhenToUse` → omit the suffix. Empty input → empty string (so the caller can `if cat != "" { ... }` to gate the whole "Available Skills:" prompt section).

**Why one level deep, not arbitrary nesting**: upstream's glob is `"skills/**/SKILL.md"` (or `"**/SKILL.md"` for the URL-pulled case), so technically a skill can live at any depth. In practice every skill in upstream's reference set lives at exactly one level — `skills/<name>/SKILL.md`. Restricting to depth 1 keeps the discovery contract obvious and the test surface small. A future `DiscoverSkillsRecursive` is one filepath.Walk call away if anyone wants the upstream behavior.

**Why last-wins on duplicates, not first-wins**: mirrors upstream L116-L122. The reasoning: caller passes `dirs` in priority order with the highest-priority root LAST. Project-local skills (most specific) come after user-home skills (less specific). When a project ships a `git-helper` that overrides the global one, the project version is what the user wants — last-wins makes that the default.

**Why we fail-loud on parse errors, not silently skip**: upstream's `add()` at L114 silently returns when frontmatter is malformed (logs a warning at L101-L106 then ignores the file). For a teaching repo that's the wrong call — the reader running `go test` shouldn't have to wonder why their SKILL.md got ignored. We return an error that names the file. Production opencode logs and continues; we trade that for a clearer failure mode.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s11 skill discovery pipeline                                          │
│                                                                        │
│   dirs = ["~/.opencode/skills", ".opencode/skills"]                    │
│           ↑ low priority             ↑ high priority (last)            │
│                                                                        │
│   DiscoverSkills(dirs)                                                 │
│     for root in dirs:                                                  │
│       for sub in os.ReadDir(root):       ← one level deep              │
│         if exists(<root>/<sub>/SKILL.md):                              │
│           skill = ParseSkillMD(...)                                    │
│           byName[skill.Name] = skill     ← LAST-WINS on collision      │
│                                                                        │
│   ParseSkillMD(path, content)                                          │
│     1. content[0:3] must be "---" line   (else error)                  │
│     2. find next "---" line              (else error)                  │
│     3. yaml.Unmarshal(frontmatter, &Skill)                             │
│     4. require non-empty Name            (else error)                  │
│     5. Body = content after closing delim, leading-newline-trimmed     │
│                                                                        │
│   CatalogString(skills) → string                                       │
│     for s in skills:                                                   │
│       "- <s.Name>: <s.Description> (use when: <s.WhenToUse>)"          │
│     if WhenToUse empty: omit "(use when: ...)" suffix                  │
│     if input empty: return ""                                          │
│                                                                        │
│   ┌──────────────────────────────────────┐                             │
│   │  s10's loop (later) prepends:        │                             │
│   │    cat = CatalogString(skills)       │                             │
│   │    if cat != "":                     │                             │
│   │      systemPrompt += "\n## Available Skills\n" + cat               │
│   └──────────────────────────────────────┘                             │
└────────────────────────────────────────────────────────────────────────┘
```

**Four load-bearing decisions**:

1. **One level deep**. `<root>/<skill-name>/SKILL.md`. Not zero-level (no `<root>/SKILL.md`), not two-level (no `<root>/<a>/<b>/SKILL.md`). Restricts the test surface to a single shape; matches every real skill in upstream's reference set; trivial to lift to `filepath.Walk` if needed.
2. **Last-wins on duplicate name, across dirs**. Caller passes priority-ordered `dirs`, lowest first. Project overrides global; agent-specific overrides project; URL-pulled overrides everything (in the upstream layout). Within a single dir, sub-dir name is sorted, so the order is deterministic.
3. **Fail-loud on parse error**. A SKILL.md without `---` or without `name` returns an error wrapping the file path. Upstream logs and skips; we trade that for a clearer test signal. (A wrapper that logs-and-continues is one `errors.Is` check away.)
4. **Catalog format is structural, not policy**. `CatalogString` doesn't sort, doesn't filter by agent permission, doesn't deduplicate. It just renders. The caller (s10's loop, future) does the policy work — sorting alphabetically before the call, filtering with `Permission.evaluate("skill", name, agent.permission)` ahead of time, etc. Keep formatting separate from policy.

**Why ~350 LOC (including tests)**: because the work is small. The Skill struct is 5 fields. ParseSkillMD does one line-scan plus `yaml.Unmarshal`. DiscoverSkills does one `os.ReadDir` plus a loop. CatalogString is a `strings.Builder`. The 5 tests probe every path. No I/O orchestration, no Effect wrapping, no Service/Layer plumbing — Go's standard library plus `gopkg.in/yaml.v3` is enough.

## What Changed (vs. s08)

s08 loaded `opencode.json` into a `Config` struct. s11 takes one of those config fields — the user's configured skill paths — and turns them into a system-prompt fragment. The Config plumbing didn't move; the new layer is purely "given the resolved skill dirs from Config, walk them and produce a catalog."

```diff
 // s08: Config exposes the user's configured skill dirs (and other paths).
 cfg, _ := Load(cwd, homeDir, EnvFromOS())
 // cfg.SkillPaths is a []string of dirs to scan for SKILL.md files.

+// s11: walk those dirs, build the catalog, splice it into the system prompt.
+skills, err := DiscoverSkills(cfg.SkillPaths)
+if err != nil {
+    return fmt.Errorf("skill discovery: %w", err)
+}
+catalog := CatalogString(skills)
+if catalog != "" {
+    systemPrompt += "\n\n## Available Skills\n" + catalog
+}
+// systemPrompt is then handed to the s10 loop's first request.
```

The shape of `Config` didn't change a line — s08's "Config is dumb data" decision keeps paying dividends. s11 ADDs a consumer of one of Config's fields. Symmetric to how s09 ADDed an agent-cascade consumer of `cfg.Permissions` without changing the Config struct.

What s10 does next: when assembling the system prompt for each request, prepend `CatalogString(skills)`. The model sees a structured menu of skills and can decide "I should use git-helper" without the user having to mention every skill explicitly. Picking a skill triggers a (future) `read_skill` tool that loads the body — the catalog only carries enough metadata for the model to pick.

## Try It

```bash
cd agents/s11-skills

# Demo (deterministic, no network):
go run .

# 5 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 5 tests cover:

1. **ParseSkillMDValid** — every field on `Skill` is populated from the right source (frontmatter → Name/Description/WhenToUse, body content → Body, argument → Path). Body must NOT contain frontmatter remnants.
2. **DiscoverSkillsOneLevelDeep** — finds `<root>/<skill>/SKILL.md`; ignores `<root>/SKILL.md` (zero-level) and `<root>/<a>/<b>/SKILL.md` (two-level). Pins the structural promise.
3. **DiscoverSkillsLastWinsOnDuplicateName** — when two roots ship a `git-helper` with different descriptions, the LATER root in `dirs` wins. Reversing `dirs` flips the winner. This is the only test that pins cross-dir order semantics.
4. **ParseSkillMDMissingFrontmatter** — three sub-cases (no leading `---`, no closing `---`, missing `name`). Each returns an error containing the file path in its message.
5. **CatalogStringFormat** — pins the exact line format `- <name>: <description> (use when: <when_to_use>)`; verifies all three frontmatter fields surface in the rendered output; verifies empty WhenToUse omits the suffix; empty input returns the empty string.

## Upstream Source Reading

s11 mirrors `packages/opencode/src/skill/index.ts` in opencode. The full file is 323 lines covering Effect-Service/Layer wiring, external-dir scan, URL-pulled discovery, and a verbose XML catalog format. s11 takes the *core mechanism* (L36-L161): the schema, the per-file `add()` with its dedup rule, and the per-dir `scan()` glob. Everything else is plumbing.

```ts
// upstream:packages/opencode/src/skill/index.ts L36-L161

// L36-L42 — runtime Info schema. Our Go `Skill` struct keeps these
// four fields (name, description, location → Path, content → Body)
// and adds `WhenToUse` so the catalog can render a structured trigger
// hint. Upstream stuffs the trigger inside `description` instead.
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  location: Schema.String,
  content: Schema.String,
})

// L52-L58 — frontmatter validator. Only `name: string` is required;
// description is optional. Our ParseSkillMD enforces the same.
function isSkillFrontmatter(data: unknown): data is { name: string; description?: string } {
  return (
    isRecord(data) &&
    typeof data.name === "string" &&
    (data.description === undefined || typeof data.description === "string")
  )
}

// L94-L131 — `add()`: parse one SKILL.md, attach to state.
//   1. ConfigMarkdown.parse splits frontmatter from body (we handroll
//      the same `---` scan in ParseSkillMD).
//   2. L114 silently skips malformed frontmatter (we fail-loud).
//   3. L116-L122 LAST-WINS on duplicate name (logs warn, then overwrites).
//      Our DiscoverSkills replicates this byte-for-byte.
const add = Effect.fnUntraced(function* (state: State, match: string, bus: Bus.Interface) {
  const md = yield* Effect.tryPromise({
    try: () => ConfigMarkdown.parse(match),
    catch: (err) => err,
  })
  if (!md) return
  if (!isSkillFrontmatter(md.data)) return                  // ← silently skipped upstream

  if (state.skills[md.data.name]) {
    log.warn("duplicate skill name", {                       // ★ last-wins:
      name: md.data.name,                                    //   warn, then overwrite
      existing: state.skills[md.data.name].location,
      duplicate: match,
    })
  }

  state.dirs.add(path.dirname(match))
  state.skills[md.data.name] = {                             // ← store the Info
    name: md.data.name,
    description: md.data.description,
    location: match,
    content: md.content,
  }
})

// L133-L161 — `scan()`: glob a single root for SKILL.md, accumulate
// matches into ScanState. Pattern is `"skills/**/SKILL.md"` (depth 2+)
// or `"**/SKILL.md"` (any). Our Go DiscoverSkills restricts to depth-1
// (`<root>/<sub>/SKILL.md`) for teaching simplicity.
const scan = Effect.fnUntraced(function* (
  state: ScanState,
  root: string,
  pattern: string,
  opts?: { dot?: boolean; scope?: string },
) {
  const matches = yield* Effect.tryPromise({
    try: () =>
      Glob.scan(pattern, {
        cwd: root,
        absolute: true,
        include: "file",
        symlink: true,
        dot: opts?.dot,
      }),
    catch: (error) => error,
  })

  for (const match of matches) {                             // ★ accumulate per-file
    state.matches.add(match)
    state.dirs.add(path.dirname(match))
  }
})
```

Line-by-line annotation (key lines):

- **L36-L42 `Info` schema** — the *shape* of a runtime skill. Our `Skill` struct is a superset (we add `WhenToUse`). The `location` field is the absolute path on disk; we mirror as `Path`. The `content` field is the markdown body after frontmatter removal; we mirror as `Body`.
- **L52-L58 `isSkillFrontmatter`** — the only HARD requirement is a string `name`. Our ParseSkillMD enforces the same in Go: missing `name` → error. Description and our extension `when_to_use` are optional.
- **L114 `if (!isSkillFrontmatter(md.data)) return`** — silently skip malformed frontmatter. Our Go side returns an error here. Upstream's call site catches errors and logs (L98-L108); we choose to surface them so a `go test` failure is self-explaining.
- **L116-L122 duplicate-name handling** — the load-bearing line. Already-existing skill name in state → log warn, OVERWRITE. Last-wins. Order of operations is determined by the caller (L173-L221's `discoverSkills`): external dirs first, config dirs next, URL-pulled dirs last. Our Go `DiscoverSkills` takes the dir list directly so the caller controls ordering.
- **L125-L130 store** — `state.skills[name] = { name, description, location, content }`. Our Go `byName[s.Name] = s` does the same; the byName map is the authoritative state, the `firstSeenOrder` slice preserves the iteration order across overwrites so the returned slice is stable.
- **L133-L161 scan glob** — the actual disk walk. Upstream uses Bun's `Glob.scan` with the upstream pattern; we use `os.ReadDir` over each root and check for `SKILL.md` in each direct sub-dir. Same effect for the depth-1 case (which is every real skill); much simpler in Go without a dependency.

Permalinks:

- Info schema (L36-L42): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L36-L42>
- isSkillFrontmatter (L52-L58): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L52-L58>
- add() per-file parse + last-wins (L94-L131): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L94-L131>
- scan() per-root glob (L133-L161): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L133-L161>

What we kept, what we cut:

- **Kept** — Info schema's load-bearing fields (name, description, location, content), `isSkillFrontmatter`'s name-required validation, last-wins on duplicate name, per-root scan, depth-1 discovery (all real skills live there).
- **Cut for now** — external-dir scan (`.claude/`, `.agents/` plus the up-walk to worktree, L173-L191), URL-pulled discovery (L210-L215), Effect-typed Service/Layer wiring (L232-L294), Bus.publish error notifications, the built-in `customize-opencode` skill (L21-L34, L250-L257), Permission-filtered `available(agent)` (L277-L282), the verbose XML catalog format (L298-L313).
- **Forward-compat** — adding fields to `Skill` (e.g. `Version` or `License`) doesn't break parse or discovery. Adding a `DiscoverSkillsRecursive(dirs []string)` for arbitrary-depth scan doesn't touch the depth-1 contract. Adding a `Filter(skills, agent)` for permission-cascade trimming (s09 + s10's territory) is a separate function — doesn't alter discovery output.

opencode skill-layer reading order:

1. `packages/opencode/src/skill/index.ts` L36-L42 — the `Info` Schema (s11's Skill struct mother)
2. `packages/opencode/src/skill/index.ts` L94-L131 — per-file `add()` with last-wins (s11's DiscoverSkills core)
3. `packages/opencode/src/skill/index.ts` L133-L161 — per-root `scan()` glob (s11's DiscoverSkills outer loop)
4. `packages/opencode/src/skill/index.ts` L296-L321 — `fmt(list, opts)` catalog renderer (s11's CatalogString simpler form)
5. `packages/opencode/src/config/markdown.ts` — `ConfigMarkdown.parse` frontmatter splitter (s11's ParseSkillMD handrolled equivalent)
