# s09 — agent-registry

s04 evaluated permissions against a flat in-memory ruleset. s08 lifted
that ruleset out of code and into `opencode.json`. s09 adds the missing
piece between them: a **named bundle** — model + system prompt +
permission cascade — that the user picks at session start. opencode
calls these *agents*: `build` (default, wide-open), `plan` (read-only),
`general` (multi-step subagent), plus the user's own `cfg.agent.<name>`
declarations from s08's loaded Config.

The load-bearing mechanism is the **three-layer permission cascade**:

```
defaults  →  userConfig  →  agentOverride
```

`MergePermissions` concatenates the three slices in argument order. The
s10 evaluator (a port of s04 / upstream's `Array.findLast`) reads the
result as last-match-wins, so the agent's own rules — which appear LAST
— always have the final say. Identical semantics to upstream's
`Permission.merge(defaults, fromConfig({...}), user)` calls in
`packages/opencode/src/agent/agent.ts` L128 / L143 / L165 / L235 / etc.

## Files

- `permission.go` — `Action` + `Rule` re-implemented locally (mirrors s04
  and s08, no cross-session imports). Same shape; lives here so the
  registry can produce `[]Rule` cascades without dragging the s04 module
  in. JSON-tagged so a future s10 can hand a Config slice straight in.
- `agent.go` — the meat:
  - `Mode` enum (`ModePrimary`, `ModeSubagent`, `ModeAll`).
  - `Agent` struct: 6 fields (Name, Mode, Model, System, Permissions,
    Tools). Mirrors upstream's `Info` schema, dropped to the
    behavior-load-bearing subset (no color / temperature / hidden / etc.).
  - `Registry` struct + `NewRegistry()` — pre-populated with the three
    canonical built-ins (build, plan, general). Each built-in's
    Permissions is the *already-merged* cascade so a freshly-constructed
    Registry is immediately usable in tests.
  - `Register(*Agent) error` — installs an agent, OVERRIDING any
    built-in (or prior registration) with the same name. Mirrors
    upstream's `cfg.agent` loop at agent.ts L282-L304.
  - `Get(name)` — comma-ok lookup.
  - `ListByMode(m Mode)` — filters by mode, sorted by Name. ModeAll
    agents do NOT auto-promote into a Primary listing — the contract is
    "ask for primary, get exactly the primaries."
  - `MergePermissions(defaults, userConfig, agentOverride)` — flat
    concat in argument order. Returns nil if all three are empty.
- `main.go` — short demo. Builds a Registry, lists primary agents,
  overrides `plan` with a custom system prompt, runs MergePermissions
  for the `build` agent across three layers, prints each layer's rules.
- `agent_test.go` — 4 tests, all `t.TempDir()`-free (pure data):
  1. **BuiltinAgentsResolve** — `Get("build" | "plan" | "general")`
     returns each, with the right Mode and a non-empty Permissions slice.
  2. **UserDefinedAgentOverridesBuiltin** — registering an agent named
     "build" with a different model fully replaces the built-in.
  3. **MergePermissionsConcatenatesInOrder** — asserts the literal-concat
     output (defaults ++ user ++ agent) and verifies the returned slice
     is independent of the inputs (no aliasing).
  4. **ListByModeReturnsOnlyMatching** — primary set is exactly
     {build, plan}; subagent set is empty before Register, then surfaces
     the registered subagent; ModeAll listing returns only `general`.

## Run

```
# Demo (deterministic, no network)
go run .

# 4 tests
go test -count=1 ./...

# Vet + build + test in one go
go vet ./... && go build ./... && go test -count=1 ./...
```

## Key teaching points

- **Three layers, deterministic order.** The cascade is
  `defaults → userConfig → agentOverride`. Concatenation is structural;
  the evaluator (s10's port of s04) does the semantic last-match-wins
  walk. Don't dedupe — that would erase the audit trail showing which
  layer made the final call.
- **Override, don't patch.** `Register` replaces wholesale. Upstream
  does field-level patch-merge in its `cfg.agent` loop, but s09 pushes
  that decision to the call site — copy from a built-in then mutate, or
  start fresh. Simpler contract; one place to look up "what's the model
  for agent X?"
- **Built-in Permissions are pre-merged.** `NewRegistry()` returns
  agents whose Permissions are already the result of a default-cascade,
  so simple cases work without calling MergePermissions explicitly.
  The function is there for the other case: a user wanting to compose
  their own cascade with a known config layer.
- **Mode is a filter, not a hierarchy.** `ListByMode(ModePrimary)` does
  NOT include ModeAll agents. If your CLI wants the union, call twice.
  Symmetric contract avoids surprises when a user's "all"-mode agent
  unexpectedly leaks into a primary-only chooser.

See `docs/zh/s09-agent-registry.md` and `docs/en/s09-agent-registry.md`
for the long-form walkthrough plus the upstream `agent.ts` excerpt that
motivates the cascade order.
