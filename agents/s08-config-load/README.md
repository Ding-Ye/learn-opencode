# s08 — config-load

s04 hard-coded permission rules in test literals. s07 stored sessions but
left every other knob — model, instructions, MCP servers, skill paths —
implicit. s08 gives the agent a *configuration* it can read off disk:
walk upward from `cwd` looking for `.opencode/opencode.json`, layer the
user's `~/.opencode/opencode.json` underneath, and let env vars
(`OPENCODE_MODEL`, `OPENCODE_PROVIDER`) win over both. Pure stdlib —
no JSON-schema, no `mergo`, no jsonc-parser dependency.

The merge isn't naive override-wins-everywhere; it has one explicit
exception: `Instructions[]` *concatenates* (user-first, project-after,
deduped) instead of being replaced. Every other slice and scalar follows
the standard "override wins if non-empty" rule that opencode's
`mergeDeep` (from `remeda`) gives for free.

## Files

- `permission.go` — `Action` + `Rule` re-implemented from s04 (snake_case
  JSON tags + a custom `UnmarshalJSON` that maps `"allow"`/`"deny"`/`"ask"`
  strings onto the enum). Same shape as s04; lives here so this module
  has zero cross-session imports.
- `paths.go` — `DefaultHomeDir()` (honors `OPENCODE_CONFIG_DIR`),
  `findProjectConfig(cwd)` (walks upward to filesystem root), and
  the `.jsonc` / `.json` candidate enumeration.
- `config.go` — the meat:
  - `Config` struct with 7 fields (Provider, Agents, Permissions,
    Instructions, LSP, MCP, Skills) — the subset of upstream's ~30-field
    `Info` that downstream sessions (s09 / s10 / s11 / s12 / s13) will
    actually consume.
  - `Load(cwd, homeDir, env)` — runs the three-stage pipeline.
  - `mergeConfigs(base, override)` — deep merge with the
    `Instructions[]`-concat exception.
  - `loadOne` + `stripJSONC` — JSONC support without a dep, via a 30-line
    state machine that preserves string contents (so `"//"` inside a
    value is untouched).
  - `applyEnvOverrides` — `OPENCODE_PROVIDER` / `OPENCODE_MODEL` win
    over both files.
- `main.go` — short demo. Builds two temp dirs (mock `~/.opencode/` and
  mock `<cwd>/.opencode/`), writes JSON into each, calls Load with an
  env override, prints the merged Config. Deterministic, no network.
- `config_test.go` — 5 tests, all using `t.TempDir()`:
  1. **ProjectOnlyConfig** — empty homeDir → only project values land.
  2. **UserOnlyConfig** — empty project → only user values land.
  3. **ProjectOverridesUser** — overlapping field; project wins; the
     non-overlapping user fields survive.
  4. **InstructionsConcatenated** — user + project both list paths;
     merged result is the union (user-first), deduped.
  5. **EnvOverrideOfProviderModel** — `OPENCODE_MODEL` env var wins
     over both files; absent env var leaves the file merge intact.

## Run

```
# Demo (deterministic, no network)
go run .

# 5 tests
go test -count=1 ./...

# Vet + build + test in one go
go vet ./... && go build ./... && go test -count=1 ./...
```

## Key teaching points

- **Three layers, deterministic order.** project ⊕ user ⊕ env, with
  project beating user and env beating both. The order matters because
  later layers can't see what earlier ones wrote — they just overwrite.
  Keep this order matching upstream's `loadGlobal` → walk-project → env
  pipeline so debugging across the two repos doesn't shift cognitive
  state.
- **Array concat is the load-bearing exception.** `Instructions[]` is
  the only slice that concatenates (deduped) instead of being replaced.
  If we replaced, a user's global `~/CLAUDE.md` would silently disappear
  the moment any project added even one of its own — a worst-case
  silent data-loss shape for system-prompt fragments.
- **Walk upward from cwd.** A config in `~/projects/foo/.opencode/`
  applies to *any* subdirectory of foo, not just foo itself. Mirrors
  upstream's `afs.up({ targets: [".opencode"], start: directory })`.
  Stops at filesystem root via the `parent == dir` check.
- **JSONC support, no dep.** opencode lets users write `//` comments
  in `opencode.jsonc`. We strip them with a 30-line state machine
  before `json.Unmarshal`. Preserves string contents and newlines (so
  parser error line numbers match the source).
- **Pure stdlib.** No `mergo`, no schema lib, no jsonc-parser. The
  whole module is `encoding/json` + `os` + `path/filepath` + `strings`.
  450 LOC including tests, no `go.sum`. Trivially auditable.

See `docs/zh/s08-config-load.md` and `docs/en/s08-config-load.md`
for the long-form walkthrough plus the upstream `config/config.ts`
excerpt that motivates the merge semantics.
