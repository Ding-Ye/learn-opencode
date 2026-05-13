---
title: "s08 · Config loading"
chapter: 8
slug: s08-config-load
est_read_min: 11
---

# s08 · Config loading

> What this chapter teaches: lift every configurable knob the agent has — model, permission rules, instruction-file paths, MCP server list, LSP server list, skill directories — out of code and into `opencode.json`. Three layers of source: project (walk up from cwd looking for `.opencode/opencode.json`), user (`~/.opencode/opencode.json`), and env override (`OPENCODE_MODEL` etc.). Deep-merged in deterministic order. `Instructions[]` *concatenates*; everything else *overrides*. Pure stdlib, zero deps.

---

## Problem

Up through s07, every configurable thing the agent does was hard-coded:

- s04's permission ruleset was an inline literal in tests: `[]Rule{{...}}`.
- s05's provider/model was hard-coded in `main` as `"claude-3-5-sonnet"`.
- s06 / s07 didn't surface anything configurable yet.

But take one more step:
- s09 needs to look up model + permission cascade by agent name — users have to be able to write "for this build agent use sonnet, for the scout agent use haiku" in `opencode.json`.
- s10's tool loop needs to evaluate permissions — the rules must come from *configuration*, not code.
- s11's SKILL.md discovery needs to know which directories to scan — that comes from the config's `skills.paths`.
- s12 / s13 spawn MCP / LSP child processes — which ones to spawn comes from config.

opencode's config layout is the familiar two-layer + env override:

| Layer | File | Role |
|---|---|---|
| project | `<cwd or ancestor>/.opencode/opencode.json` | Project-specific overrides; auto-found from any subdirectory. |
| user    | `~/.opencode/opencode.json`                  | Per-user global default; shared across all projects. |
| env     | `OPENCODE_PROVIDER` / `OPENCODE_MODEL`       | One-shot runtime override. |

The merge isn't "project replaces user wholesale" — it's a **deep merge**: if the project changes `Provider.ModelID`, the `Provider.ProviderID` the user set still survives. `Skills` is a map, so the keys union from both sides. The load-bearing exception: **`Instructions[]` concatenates (with dedup), it doesn't replace** — the user's global `~/CLAUDE.md` must not silently disappear the moment a project adds even one `AGENTS.md`.

Why not pull in `mergo` or similar? Because the deep-merge semantics aren't "I want mergo's defaults" — they're "I want each field to be *explicit* about whether it overrides or concatenates." Writing the 30-line `mergeConfigs` out by hand is simpler than depending on a library + having to explain in docs that "we use `mergo.WithAppendSlice` but `Instructions` is the exception."

## Solution

A type plus one entry point:

```go
type Config struct {
    Provider     ProviderConfig
    Agents       []AgentConfig
    Permissions  []Rule         // s04's Rule, redefined locally in this module
    Instructions []string       // ★ concat field
    LSP          map[string]LSPConfig
    MCP          []MCPConfig
    Skills       map[string]string
}

func Load(cwd, homeDir string, env map[string]string) (*Config, error)
```

Three-stage pipeline:

1. **user**: read `<homeDir>/.opencode/opencode.json{,c}` (missing → empty Config, no error)
2. **project**: walk upward from cwd, return on the first `.opencode/opencode.json{,c}` (same: not-found → empty Config)
3. **env**: in-place apply `OPENCODE_PROVIDER` / `OPENCODE_MODEL` to the merged result

The merge: `mergeConfigs(user, project)` — user is base, project is override, so project beats user. Env is applied last, so env beats everything.

**The 7 rules in `mergeConfigs`**:

| Field | Rule |
|---|---|
| `Provider.{ProviderID,ModelID}` | per-field override-if-non-empty (not whole-struct replacement) |
| `Agents` | slice replaced wholesale if override is non-empty |
| `Permissions` | slice replaced wholesale if override is non-empty |
| `Instructions` | **base ++ override, then dedup (first-occurrence preserved)** |
| `LSP` | map union; override key wins on collision |
| `MCP` | slice replaced wholesale if override is non-empty |
| `Skills` | map union; override key wins on collision |

**JSONC support**: opencode lets users write `// line comments` / `/* block comments */` in `opencode.jsonc`. We strip them with a 30-line state machine `stripJSONC` before passing to `json.Unmarshal`. The state machine treats string literals as a first-class state, so `"key": "// not a comment"` is left alone. Zero deps.

**Why walk upward**: a user runs `cd ~/projects/foo/sub/dir` then `opencode`, expecting `~/projects/foo/.opencode/opencode.json` (if it exists) to take effect — not requiring a config in every subdirectory. We start from cwd, `filepath.Dir` each step, until the root.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s08 three-layer config merge                                           │
│                                                                        │
│   Load(cwd, homeDir, env)                                               │
│     │                                                                  │
│     ├─ user     ←  loadOptional(<homeDir>/.opencode/opencode.{jsonc,json})│
│     │             ↓ missing → Config{}                                  │
│     │                                                                  │
│     ├─ project  ←  walk-up from cwd → first `.opencode/opencode.*` hit  │
│     │             ↓ not-found → Config{}                                │
│     │                                                                  │
│     └─ merged   =  mergeConfigs(user, project)  ← project beats user    │
│                                                                        │
│           applyEnvOverrides(&merged, env)        ← env beats everything │
│                                                                        │
│           return &merged                                                │
│                                                                        │
│   mergeConfigs(base, override):                                         │
│     - scalars / nested-struct fields → override-if-non-empty            │
│     - slices (Agents / Permissions / MCP) → replaced if override non-empty│
│     - Instructions[] → base ++ override, dedup keeping first occurrence │
│     - maps (LSP / Skills) → shallow union, override wins                │
└────────────────────────────────────────────────────────────────────────┘
```

**Four load-bearing decisions**:

1. **"override-if-non-empty" semantics.** Go's zero values (`""`, `nil`) play the role of TS's `undefined` — a config that didn't set `model` should *not* zero out the `model` set by the layer below. This rule lets the user config focus on "my global defaults" and the project config focus on "what's different from my defaults," without either having to copy every field of the other.
2. **`Instructions[]` is the concat exception.** Upstream's `mergeConfigConcatArrays` uses `Array.from(new Set([...target, ...source]))`; we use `dedupStrings(append(base, override...))`. Identical semantics. Replace-instead-of-concat would mean a user's `~/CLAUDE.md` silently vanishes the moment any project adds even one `AGENTS.md` — worst-case silent data loss for system-prompt fragments.
3. **walk-upward MUST stop at the filesystem root.** `filepath.Dir("/") == "/"` (macOS / Linux), `filepath.Dir("C:\\") == "C:\\"` (Windows) — without an explicit terminator, the loop runs forever. We use the `parent == dir` idiom; this is the cross-platform Go walk-up convention.
4. **env override is applied *after* merge.** If we applied it before merging, the project's `model` would re-overwrite the env-set `model`, making the env override useless. The order is load-bearing; test #5's "no-env fallback" assertion pins exactly this property.

**Why ~450 LOC (including tests)**: because it does only four things — find files, read JSON, merge, apply env. No schema validator (`encoding/json` struct tags suffice), no `mergo` (30 lines hand-rolled is more explicit), no jsonc-parser (a 30-line state machine is enough), no plugin resolution / `${VAR}` templating / file watcher (all in later chapters or out of scope).

## What Changed (vs. s04)

s04's ruleset was an inline literal in tests: `[]Rule{{Permission: "edit", ...}}`. s08's ruleset comes from `Config.Permissions`, loaded from `opencode.json`:

```diff
 // s04: ruleset was an in-memory literal constructed by the caller.
 ruleset := Ruleset{
-    {Permission: "edit", Pattern: "*.go", Action: ActionAllow},
-    {Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
 }
-Evaluate("edit", "main.go", ruleset)
+
+// s08: ruleset comes from Config; Config is loaded from disk.
+cfg, _ := Load(cwd, homeDir, EnvFromOS())
+// s10 will feed cfg.Permissions to evaluate; s09 will cascade
+// each agent's AgentConfig.Permissions on top of this.
+for _, r := range cfg.Permissions {
+    fmt.Printf("%s %s -> %s\n", r.Permission, r.Pattern, r.Action)
+}
```

The shape of `Rule` didn't change a line — that's the proof that s04's "Rule is dumb data" decision was right. s08 added a *source* layer, not a shape change. `UnmarshalJSON` maps the strings `"allow"` / `"deny"` / `"ask"` onto the `Action` enum; any other value (including absent) falls back to `ActionAsk`, so a typo fails *closed* (ask the user) instead of *open* (silently grant access).

s09 will add yet another cascade on top: `globalPermissions ⊕ userOverridePermissions ⊕ agentOwnPermissions`, evaluated in order (last-match-wins — still the s04 semantics). s08 here is only responsible for getting that first `globalPermissions` layer out of JSON.

## Try It

```bash
cd agents/s08-config-load

# Demo (deterministic, no network):
go run .

# 5 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 5 tests cover:

1. **ProjectOnlyConfig** — empty homeDir (= no user config), only the project's `.opencode/opencode.json`. Provider / Instructions / Permissions all come from project.
2. **UserOnlyConfig** — cwd has no `.opencode/` anywhere, only `~/.opencode/opencode.json`. All fields come from user.
3. **ProjectOverridesUser** — both sides set `Provider.ModelID`, project wins; the `Skills` map the user set survives because the project didn't touch it. Verifies *per-field* deep merge (not whole-struct replacement).
4. **InstructionsConcatenated** — user `[~/CLAUDE.md, shared.md]` + project `[AGENTS.md, shared.md]` → merged `[~/CLAUDE.md, shared.md, AGENTS.md]` (user-first, with shared.md deduped).
5. **EnvOverrideOfProviderModel** — `OPENCODE_MODEL=claude-3-opus` beats both files; the same test also verifies that *without* the env var, project still beats user — confirming the env override is an *extra* layer, not a replacement of the merge order.

## Upstream Source Reading

s08 mirrors `packages/opencode/src/config/config.ts`. The whole file is 1500+ lines, but the deep-merge + array-concat core is just three small functions (L49-L110) — we port the semantics one-to-one in Go. The schema (L120-L292, 30+ fields) is trimmed in our Go `Config` to 7 fields; the rest will land as the owning sessions arrive.

```ts
// upstream:packages/opencode/src/config/config.ts L49-L110

// Custom merge function that concatenates array fields instead of replacing them
// Keep remeda's deep conditional merge type out of hot config-loading paths;
// TS profiling showed it dominates here.
function mergeConfig(target: Info, source: Info): Info {
  return mergeDeep(target, source) as Info
}

function mergeConfigConcatArrays(target: Info, source: Info): Info {
  const merged = mergeConfig(target, source)
  if (target.instructions && source.instructions) {
    merged.instructions = Array.from(new Set([...target.instructions, ...source.instructions]))
  }
  return merged
}

function normalizeLoadedConfig(data: unknown, source: string) {
  if (!isRecord(data)) return data
  const copy = { ...data }
  const hadLegacy = "theme" in copy || "keybinds" in copy || "tui" in copy
  if (!hadLegacy) return copy
  delete copy.theme
  delete copy.keybinds
  delete copy.tui
  log.warn("tui keys in opencode config are deprecated; move them to tui.json", { path: source })
  return copy
}

async function substituteWellKnownRemoteConfig(input: { value: unknown; dir: string; source: string }) {
  if (!isRecord(input.value) || typeof input.value.url !== "string") return
  // ...remote config URL substitution; out of scope for s08...
}

async function resolveLoadedPlugins<T extends { plugin?: ConfigPlugin.Spec[] }>(config: T, filepath: string) {
  if (!config.plugin) return config
  for (let i = 0; i < config.plugin.length; i++) {
    config.plugin[i] = await ConfigPlugin.resolvePluginSpec(config.plugin[i], filepath)
  }
  return config
}
```

Line-by-line annotation (key lines):

- **L49-L51 `mergeConfig`** — a one-line wrapper around `remeda.mergeDeep`. The comment notes "TS profiling showed this dominates the hot path; bypass remeda's deep-conditional type inference." Go doesn't have this problem (we hand-roll the loop); our `mergeConfigs` is the equivalent, listing each field's merge rule explicitly.
- **L53-L59 `mergeConfigConcatArrays`** — this is the *core* of s08. First call `mergeConfig` for the default deep-merge, then *separately* handle `instructions`: concat with `new Set([...A, ...B])` to dedup. Our Go version (the Instructions branch in `mergeConfigs` in `config.go`) does the same — `dedupStrings(append(base.Instructions, override.Instructions...))`. JS `Set` preserves insertion order; our `dedupStrings` uses map-as-set + an output slice that also preserves first-occurrence order. Identical behavior.
- **L61-L71 `normalizeLoadedConfig`** — strips legacy keys. `theme` / `keybinds` / `tui` used to live in opencode.json; they later moved to a sibling `tui.json`. This fn deletes them on load with a `log.warn`. On the Go side our `Config` struct doesn't declare these fields at all — `encoding/json` defaults to "silently drop unknown fields" — so this step is a no-op for us; we don't implement it.
- **L73-L101 `substituteWellKnownRemoteConfig`** — handles the `{url, headers}` form of remote config include (one config file pulls in another from a URL). s08 does no include / remote loading, pure local.
- **L103-L111 `resolveLoadedPlugins`** — resolves the relative paths inside `{ plugin: [...] }` to absolute paths so they don't shift if the config is later merged in another location. s08 doesn't do plugins (opencode's plugin system is a whole separate topic); skipped.

Partial `Info` schema (L120-L292, just the fields s08 uses):

```ts
export const Info = Schema.Struct({
  // ...
  provider: Schema.optional(Schema.Record(Schema.String, ConfigProvider.Info)),  // we simplify to {ProviderID, ModelID}
  agent:    Schema.optional(/* ... */),                                          // our Agents []AgentConfig
  permission: Schema.optional(ConfigPermission.Info),                            // our Permissions []Rule
  instructions: Schema.optional(Schema.mutable(Schema.Array(Schema.String))),     // our Instructions []string ★ concat
  lsp:    Schema.optional(ConfigLSP.Info),                                       // our LSP map
  mcp:    Schema.optional(/* ... */),                                            // our MCP []MCPConfig
  skills: Schema.optional(ConfigSkills.Info),                                    // our Skills map
  // ...23 more fields, not yet in s08, each owned by a future session
})
```

Permalinks:

- mergeConfig + mergeConfigConcatArrays (L49-L60): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L49-L60>
- normalizeLoadedConfig + remote / plugin (L61-L111): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L61-L111>
- Full Info schema (L120-L292): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L120-L292>
- paths.ts (walk-up search): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/paths.ts>

What we kept, what we cut:

- **Kept** — three layers (user / project / env), deep-merge semantics, `Instructions[]` concat exception, JSONC support, walk-upward project search, `OPENCODE_CONFIG_DIR` / `OPENCODE_PROVIDER` / `OPENCODE_MODEL` env hooks.
- **Cut for now** — `${VAR}` string templating (s11 skill discovery will), remote config include, plugin resolution, `$schema` auto-injection, `tui.json` legacy migration, effect/Schema runtime validation (replaced by Go struct tags), file watcher / hot reload, Auth / Account / Npm deps.
- **Forward-compat** — adding a new field to `Config` doesn't perturb `mergeConfigs`, because the merge rules are sorted by field type (scalar / slice / map); adding `Compaction CompactionConfig` is one new override line. `Agents []AgentConfig` is already in the schema — s09 will read `cfg.Agents` and construct runtime `Agent`s from them. s10 / s11 / s12 / s13 likewise.

opencode config-layer reading order:

1. `packages/opencode/src/config/config.ts` L49-L110 — merge fns + normalize (s08's mirror core, this section's body)
2. `packages/opencode/src/config/config.ts` L120-L292 — full `Info` schema (s08 takes a 7-field subset)
3. `packages/opencode/src/config/paths.ts` — `directories` / `files` walk-up search (s08 mirrors in `paths.go`)
4. `packages/opencode/src/config/config.ts` L376-L450 — full `loadConfig` / `loadFile` / `loadGlobal` pipeline (s08 mirrors in `Load`)
5. `packages/opencode/src/config/parse.ts` — JSONC parsing + Schema validation (we replace with stdlib `encoding/json` + struct tags)
