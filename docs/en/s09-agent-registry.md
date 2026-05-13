---
title: "s09 · Agent registry"
chapter: 9
slug: s09-agent-registry
est_read_min: 11
---

# s09 · Agent registry

> What this chapter teaches: bundle a `(model, system prompt, permission ruleset, mode)` together as a named *agent*, build a Registry pre-loaded with three built-ins (build / plan / general), and let users override them via `opencode.json`. The load-bearing mechanism is the **three-layer permission cascade**: `defaults → userConfig → agentOverride`, concatenated in order; s10's evaluator walks last-match-wins, so the agent's last word always beats every layer above.

---

## Problem

Through s08, permission rules can be loaded from `opencode.json`, but only as *one* slice: a single global `cfg.Permissions`. That doesn't cover the real-world cases:

- A user wants the same project to have a "normal build" agent (can edit) AND a "plan mode" (read-only) coexist as siblings, not as a config switch.
- opencode's `task` tool dispatches a *subagent* to do focused subtasks ("find every TODO comment under src/") — the subagent shouldn't inherit the parent agent's edit permission.
- A user writes their own agent ("researcher", grep + read + websearch only) and expects `--agent researcher` to just work.

opencode's answer is **agents**: each agent is a named bundle of `(name, mode, model, system prompt, permission ruleset)`; a user's `cfg.agent.<name>` block overrides a built-in by the same name, or installs a new one. Permissions aren't from a single source — they're *cascaded*:

| Layer | Source | Role |
|---|---|---|
| defaults | hard-coded baseline in code | "everything allow by default; deny a few high-risk ones" |
| userConfig | s08's `cfg.Permissions` | user's global override ("ask before any edit, anywhere") |
| agentOverride | the agent's own `permissions[]` | per-agent override ("plan agent denies all edits") |

Concatenated in order, **s10's evaluator runs last-match-wins** (the s04 semantic) — the agent's last word always beats every layer above. That's the cascade.

To spell it out: upstream's `Permission.merge(defaults, fromConfig({...}), user)` is a function signature; the argument order IS the cascade order. Our Go `MergePermissions(defaults, userConfig, agentOverride)` mirrors it (with the agent layer LAST, because once a user has authored the agent's permissions block, that block IS the agent's "final say"; upstream put `user` last because the user config is the user's last word — both orders preserve the load-bearing guarantee that the *last layer wins*; we picked agent-last because `cfg.agent.<name>.permission` is what a user writes when they want this specific agent to behave a specific way).

## Solution

A type plus a Registry:

```go
type Mode int
const (ModePrimary Mode = iota; ModeSubagent; ModeAll)

type Agent struct {
    Name        string
    Mode        Mode
    Model       string
    System      string
    Permissions []Rule    // ★ the cascade RESULT, not the raw layers
    Tools       []string  // optional whitelist; nil = all available tools
}

type Registry struct { agents map[string]*Agent }

func NewRegistry() *Registry           // pre-loads 3 built-ins
func (r *Registry) Register(a *Agent) error
func (r *Registry) Get(name string) (*Agent, bool)
func (r *Registry) ListByMode(m Mode) []*Agent

func MergePermissions(defaults, userConfig, agentOverride []Rule) []Rule
```

Three built-in agents, mirroring upstream agent.ts L122-L175:

| Agent | Mode | Role | Permissions (already cascaded) |
|---|---|---|---|
| build | Primary | default; can run any tool | `[*:* allow]` — wide open |
| plan | Primary | read-only; produce plans, don't touch files | `read/grep/glob:* allow`, `edit/write/bash:* deny` |
| general | All | multi-step exploration (subagent-friendly) | `*:* allow`, but `edit/write:* ask`, `bash:rm -rf* deny` |

Each built-in's Permissions is the *already-merged* final cascade — so the caller can pull a built-in out of the Registry and use it without writing a line of merge code. To layer additional user config on top, call `MergePermissions(defaults, userConfig, builtIn.Permissions)` explicitly.

**Register is *wholesale replacement*, not patch-merge.** Upstream's agent.ts L282-L304 loop does field-by-field patch (each field is read from cfg.agent, missing ones inherit from the built-in); we simplify to total replacement: callers decide which built-in to copy from. A clean simplification — callers who want to inherit `Get` a built-in, copy, mutate, then `Register`; callers who want to start fresh just `Register(&Agent{...})` directly. Patch hides "which field defaults from where" inside the registry; replacement makes it explicit at the call site.

**`MergePermissions` is *flat concat*, not dedupe.** The tempting refactor: collapse rules with the same `(Permission, Pattern)` to keep only the last — the result is equivalent for a last-match-wins evaluator. But dedupe erases the *cascade audit trail* — reading the merged slice you can't tell anymore that "the agent's deny overrode the user's allow." Concat keeps both layers simple: this function is purely structural, the evaluator does the semantic work.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s09 three-layer permission cascade                                    │
│                                                                        │
│   NewRegistry()                                                        │
│     ├─ build  (ModePrimary,  Permissions = pre-merged wide-open)       │
│     ├─ plan   (ModePrimary,  Permissions = pre-merged read-only)       │
│     └─ general(ModeAll,      Permissions = pre-merged cautious-open)   │
│                                                                        │
│   Register(a *Agent)                                                   │
│     └─ agents[a.Name] = a   ← same-name wholesale override (incl. built-in)│
│                                                                        │
│   Get(name) → (*Agent, bool)                                            │
│   ListByMode(m) → []*Agent  ← sorted by Name                           │
│                                                                        │
│   MergePermissions(defaults, userConfig, agentOverride):                │
│     return defaults ++ userConfig ++ agentOverride                      │
│                                                                        │
│   ┌──────────────────────────────────────┐                              │
│   │  s10 evaluator, given the merged slice:│                            │
│   │    walk it; remember the LAST match   │                              │
│   │    no match → ActionAsk                │                              │
│   │    → ★ agent is last, so agent always wins│                          │
│   └──────────────────────────────────────┘                              │
└────────────────────────────────────────────────────────────────────────┘
```

**Four load-bearing decisions**:

1. **Cascade order is (defaults, userConfig, agentOverride) — agent is LAST.** This is the chapter's load-bearing nail. Last-match-wins means "the LAST layer has the final say"; we put the agent layer last because the agent represents the user's most specific intent for this scenario. If we ordered it (agent, user, defaults), the defaults would override the agent's explicit will — a `plan` agent's `edit:* deny` would get replaced by defaults' `edit:* ask`, breaking plan mode entirely.
2. **Built-in Permissions are *pre-merged*.** `NewRegistry()` doesn't require the caller to understand cascade — `r.Get("plan")` returns a ready-to-evaluate slice. Callers who want to layer additional user config call `MergePermissions(defaults, cfg.Permissions, builtIn.Permissions)` themselves. Make the simple case simple, the complex case explicit.
3. **Register replaces, doesn't patch.** Upstream patches (clever, but couples registry to field defaults); we replace (dumb, but caller has full control). Functionally equivalent: callers who want inheritance Get-then-mutate; callers who want a fresh start just Register. Two extra lines at the call site, one less layer of implicit behavior.
4. **ListByMode(ModePrimary) does NOT return ModeAll.** `general` is ModeAll — usable as both primary and subagent — but it does *not* sneak into `ListByMode(ModePrimary)`. Symmetric "ask for X, get exactly X" contract. Want the union? Call twice.

**Why ~400 LOC (including tests)**: because it does only four things — three built-in literals, Register/Get/ListByMode map ops, MergePermissions's three appends, main.go's demo. No LLM-driven agent generation (upstream's `generate(...)` at agent.ts L321-L460), no prompt-template loading (upstream reads from `./prompt/*.txt`; we inline strings), no explore/scout/compaction/title/summary built-ins (the latter three are internal-only).

## What Changed (vs. s04 / s08)

s04 treated the ruleset as an in-memory literal constructed by the caller; s08 lifted the ruleset into `Config.Permissions` loaded from `opencode.json`; s09 puts Permissions inside an *agent's context*:

```diff
 // s04: a single ruleset, inline literal.
-Evaluate("edit", "main.go", Ruleset{
-  {Permission: "edit", Pattern: "*.go", Action: ActionAllow},
-})

 // s08: ruleset comes from Config, but it's still a single source.
-cfg, _ := Load(cwd, homeDir, EnvFromOS())
-Evaluate("edit", "main.go", cfg.Permissions)

+// s09: ruleset lives inside an agent; we cascade three layers.
+r := NewRegistry()
+plan, _ := r.Get("plan")
+merged := MergePermissions(defaults, cfg.Permissions, plan.Permissions)
+// s10 hands merged to the evaluator: Evaluate("edit", "main.go", merged)
+// → walks last-match-wins; plan's deny beats user's allow every time.
```

The shape of `Rule` didn't change a line — s04's "Rule is dumb data" decision keeps paying dividends. s08 added a *source*; s09 adds *context (which agent)* + *layered composition (three layers)*; neither moved the Rule shape. The `Action` enum's unmarshal behavior is unchanged: a typo in JSON still falls back to `ActionAsk` (fail-closed).

What s10 does next: take the merged slice and hand it to s04's `Evaluate(permission, target, mergedSlice)`. The evaluator doesn't know "three layers" exist — it sees a flat `[]Rule` and finds the last match. That's the payoff of the *cascade is structural, evaluator is semantic* split: neither side needs to know the other's details.

## Try It

```bash
cd agents/s09-agent-registry

# Demo (deterministic, no network):
go run .

# 4 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 4 tests cover:

1. **BuiltinAgentsResolve** — `Get("build" | "plan" | "general")` all return non-nil with the right Mode and a non-empty Permissions slice. An unknown name returns `ok=false` so callers can distinguish "not configured" from "configured but empty."
2. **UserDefinedAgentOverridesBuiltin** — registering an Agent named `build` with the `openai/gpt-4o` model: `Get("build").Model` must come back as gpt-4o (wholesale override, not patch). Also pins the input validation contract (nil / empty Name → error).
3. **MergePermissionsConcatenatesInOrder** — pins the literal-concat output (defaults ++ user ++ agent) and verifies the returned slice is independent of the inputs (mutation doesn't alias). Empty inputs → nil.
4. **ListByModeReturnsOnlyMatching** — primary set is exactly `{build, plan}`; subagent set is empty before Register, surfaces the registered agent after; the ModeAll listing returns only `general` (no auto-promotion into ModePrimary).

## Upstream Source Reading

s09 mirrors `packages/opencode/src/agent/agent.ts` in opencode. The full file is 460 lines; s09 takes the *runtime registry* portion (L28-L304) — Schema, defaults ruleset, three core built-ins, the cfg.agent override loop. The remaining `generate(...)` LLM-driven agent synthesis (L321-L460) is a separate mechanism ("use an LLM to write a new agent"); s09 doesn't do it.

```ts
// upstream:packages/opencode/src/agent/agent.ts L28-L48 + L100-L175 + L282-L304

// L28-L48 — runtime Info schema. Our Go `Agent` struct keeps the 6
// load-bearing fields (name, mode, model, prompt, permission, tools)
// and drops the rendering-only ones (color, temperature, topP, variant,
// hidden, native, options, steps) — those don't affect "what the agent
// can do" semantics.
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  mode: Schema.Literals(["subagent", "primary", "all"]),
  // ...
  permission: Permission.Ruleset,  // ★ this is the cascade RESULT
  model: Schema.optional(Schema.Struct({ modelID: ModelID, providerID: ProviderID })),
  prompt: Schema.optional(Schema.String),
  // ...
})

// L100-L121 — the defaults / user pair, used by every built-in's
// Permission.merge(defaults, ..., user) call.
const defaults = Permission.fromConfig({
  "*": "allow",
  doom_loop: "ask",
  external_directory: { "*": "ask" /* + skill dir whitelisting */ },
  question: "deny",
  plan_enter: "deny",
  plan_exit: "deny",
  // ...some finer-grained read rules...
})
const user = Permission.fromConfig(cfg.permission ?? {})

// L122-L175 — the three core built-ins. Each `permission` is a
// three-layer Permission.merge(defaults, agentSpecific, user) call.
const agents = {
  build: {
    name: "build",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({ question: "allow", plan_enter: "allow" }),
      user,
    ),
    mode: "primary",
    native: true,
  },
  plan: {
    name: "plan",
    description: "Plan mode. Disallows all edit tools.",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({
        edit: { "*": "deny" /* + plan markdown whitelist */ },
        // ...
      }),
      user,
    ),
    mode: "primary",
    native: true,
  },
  general: {
    name: "general",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({ todowrite: "deny" }),
      user,
    ),
    mode: "subagent",
    native: true,
  },
}

// L282-L304 — the cfg.agent override loop. For each user-declared agent:
//   - already-existing built-in → patch (field-by-field merge)
//   - missing → install new (permission = Permission.merge(defaults, user); 2 layers)
// Our Go side simplifies to wholesale Register: caller chooses what to inherit.
for (const [key, value] of Object.entries(cfg.agent ?? {})) {
  if (value.disable) { delete agents[key]; continue }
  let item = agents[key]
  if (!item) item = agents[key] = {
    name: key, mode: "all",
    permission: Permission.merge(defaults, user),
    options: {}, native: false,
  }
  if (value.model) item.model = Provider.parseModel(value.model)
  // ...remaining fields patched one by one...
  item.permission = Permission.merge(item.permission, Permission.fromConfig(value.permission ?? {}))
}
```

Line-by-line annotation (key lines):

- **L28-L48 `Info` schema** — the *shape* of a runtime agent. Our `Agent` struct is a subset of it (drops 8 fields that don't affect behavior). `mode` is the string literal `"primary" | "subagent" | "all"`; we use a `Mode` enum + `String()` method. The `permission` field's type is `Permission.Ruleset` (a `Rule[]`); we mirror with `Permissions []Rule`.
- **L100-L121 defaults / user pair** — every built-in uses the same `defaults` (hard-coded safe baseline) and the same `user` (from `cfg.permission`). Our Go side: callers construct their own defaults []Rule (the demo does this), cfg.Permissions comes from s08.
- **L128 build's cascade** — `Permission.merge(defaults, fromConfig({question: "allow", plan_enter: "allow"}), user)`. Three layers: defaults → build's two added rules → user. Our `MergePermissions(defaults, userConfig, agentOverride)` orders as `(defaults, user, agent)` — **agent is last** rather than user. Both orderings satisfy "the last layer wins"; we picked agent-last because `cfg.agent.<name>.permission` IS the user's final intent for this specific agent (whatever the user writes in cfg.agent's permission block IS the agent's "final say").
- **L143-L161 plan's cascade** — same three layers, the agent-specific layer's content is `edit: {"*": "deny", ...}` — denies all edits, then whitelists planning markdown like `.opencode/plans/*.md`. Our plan built-in's Permissions is simplified: just `read/grep/glob:* allow` + `edit/write/bash:* deny`, skipping the whitelist.
- **L162-L175 general's cascade** — note `mode: "subagent"` (only invoked from the task tool's subagent dispatch). Our `general` is ModeAll (looser, eligible as both primary and subagent), since s09 doesn't ship the task tool — making `general` ModeAll makes it visible in the demo without breaking semantics.
- **L282-L304 cfg.agent override loop** — this is the upstream code that s09's `Register` corresponds to. Look at L286-L290: `if (!item) item = agents[key] = { ... permission: Permission.merge(defaults, user) }` — newly-installed agents get a *two-layer* permission (defaults + user, no agent-specific third layer yet, because the agent hasn't declared its own rules). Then L303 `item.permission = Permission.merge(item.permission, Permission.fromConfig(value.permission ?? {}))` folds in the agent's own permissions — that's the *third layer*. Our Go side pushes this to the caller: when constructing an Agent, decide for yourself with `Permissions: MergePermissions(defaults, user, ownRules)`.

Permalinks:

- Info schema (L28-L48): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L28-L48>
- defaults + user layers (L100-L121): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L100-L121>
- Three core built-ins (L122-L175): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L122-L175>
- cfg.agent override loop (L282-L304): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L282-L304>

What we kept, what we cut:

- **Kept** — Info schema's 6 load-bearing fields, three core built-ins (build/plan/general), three-layer cascade semantics, cfg.agent override (in the simplified Register-replaces form), three modes (Primary/Subagent/All), ListByMode filtering.
- **Cut for now** — explore / scout / compaction / title / summary built-ins (the first two are exploration-oriented, the last three are internal-only), LLM-driven `generate(...)` agent synthesis (agent.ts L321-L460), Truncate.GLOB auto-whitelist post-processing (L307-L320), prompt file loading (we inline strings), field-by-field patch-merge (replaced with wholesale Register).
- **Forward-compat** — adding new fields to `Agent` (e.g. temperature / topP) doesn't break cascade logic, because cascade only touches the Permissions field. s10 hands the cascaded slice straight to the evaluator, transparent to added fields. If field-level patch-merge ever becomes desirable (walking back s09's simplification one step), adding a `RegisterMerged(name string, override AgentOverride)` doesn't touch the existing Register signature.

opencode agent-layer reading order:

1. `packages/opencode/src/agent/agent.ts` L28-L48 — the `Info` Schema (parent of s09's Agent struct; this section's body)
2. `packages/opencode/src/agent/agent.ts` L100-L175 — defaults / user / three core built-ins' cascade (s09's mirror core)
3. `packages/opencode/src/agent/agent.ts` L282-L304 — cfg.agent override loop (s09's simplified Register form)
4. `packages/opencode/src/permission/index.ts` — `Permission.merge` / `Permission.fromConfig` implementation (s09's MergePermissions is the union-and-simplify of these two)
5. `packages/opencode/src/permission/evaluate.ts` L9-L15 — the `findLast` last-match-wins semantic (s10 hands s09's cascade slice to this evaluator)
