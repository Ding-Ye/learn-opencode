// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/agent/agent.ts (Info schema + built-in agents + cfg.agent merge, L28-L304)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L28-L304
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s09 cares about this file:
//   This is the *exact* code we Go-port. Three regions matter:
//
//     1. L28-L48 `Info` Schema — the runtime agent shape. We mirror the
//        five load-bearing fields (name, mode, model, prompt, permission)
//        and drop the rendering-only ones (color, temperature, topP,
//        variant, hidden, native, options, steps).
//
//     2. L100-L121 — the `defaults` ruleset and the `user` ruleset (from
//        `cfg.permission`). Together they form the first two layers of
//        s09's three-layer cascade. Our `MergePermissions(defaults,
//        userConfig, agentOverride)` takes the same two layers as the
//        first two args.
//
//     3. L122-L275 — the built-in agents map. Each agent's `permission:`
//        is constructed via `Permission.merge(defaults, fromConfig({...
//        agent-specific...}), user)`. That's exactly the cascade contract
//        s09's `MergePermissions` produces, just expressed via a helper
//        function instead of a slice concatenation.
//
//   We pick three of the upstream agents to ship as Go built-ins:
//   build (wide-open primary), plan (read-only primary), general (ModeAll
//   for multi-step exploration). Upstream also ships explore / scout (in a
//   feature flag) / compaction / title / summary; the latter three are
//   internal-only and out of scope for s09's teaching contract.
//
// What we rebuilt in Go (s09):
//   - `Info` Schema                     → `Agent` struct, 6 fields
//   - built-in `build` / `plan` / `general` → `builtinAgents()` in agent.go
//   - `Permission.merge(...)` cascade    → `MergePermissions(...)` in agent.go
//   - `Mode = "primary" | "subagent" | "all"` → `Mode` enum (3 constants)
//   - `cfg.agent` override loop (L282-L304) → `Registry.Register(...)` (replace
//                                              wholesale; user is responsible
//                                              for copying-then-mutating)
//   - `Service.list / get / defaultAgent` → `Registry.{Get,ListByMode}`
//
// What we DID NOT rebuild yet (lives in later sessions or out of scope):
//   - explore / scout / compaction / title / summary built-ins — out of scope
//                                                                 (the first
//                                                                 three teach
//                                                                 the cascade
//                                                                 contract)
//   - `generate(...)` LLM-driven agent synthesis (L321-L460)      — out of scope
//   - Truncate.GLOB whitelist post-processing (L307-L320)          — covered by
//                                                                    the s09
//                                                                    contract;
//                                                                    plan-mode
//                                                                    `external_directory`
//                                                                    quirks are
//                                                                    skipped
//   - Effect-typed Service / Layer / Context machinery             — Go uses a
//                                                                    plain struct
//   - prompt loading from `./prompt/*.txt` files                    — inlined as
//                                                                    Go string
//                                                                    literals
//   - skill / plugin / auth dependencies (cfg ⊃ all of them)        — owned by
//                                                                    later
//                                                                    sessions
//
// The 30-60 lines below are the heart of the agent registry: the Info
// schema, the defaults ruleset, and the build/plan/general entries with
// their `Permission.merge(...)` calls — three calls, three layers, in
// the same order our Go MergePermissions expects.

// L28-L48 — the runtime `Info` schema. Our Go `Agent` struct keeps the
// load-bearing five fields (name, mode, model, prompt, permission) and
// the optional `tools` whitelist; the rest (color, temperature, topP,
// variant, hidden, native, options, steps) are rendering-only and dropped.
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  mode: Schema.Literals(["subagent", "primary", "all"]),
  native: Schema.optional(Schema.Boolean),
  hidden: Schema.optional(Schema.Boolean),
  topP: Schema.optional(Schema.Finite),
  temperature: Schema.optional(Schema.Finite),
  color: Schema.optional(Schema.String),
  permission: Permission.Ruleset, // ★ this is the merged cascade result
  model: Schema.optional(
    Schema.Struct({
      modelID: ModelID,
      providerID: ProviderID,
    }),
  ),
  variant: Schema.optional(Schema.String),
  prompt: Schema.optional(Schema.String),
  options: Schema.Record(Schema.String, Schema.Unknown),
  steps: Schema.optional(Schema.Finite),
}).annotate({ identifier: "Agent" })
export type Info = DeepMutable<Schema.Schema.Type<typeof Info>>

// L100-L121 — the two cascade layers that show up in every built-in's
// `Permission.merge(defaults, ..., user)` call below. `defaults` is the
// hard-coded baseline (everything-allowed except a few high-blast-radius
// permissions); `user` is whatever's in cfg.permission.
//
// Our Go equivalents:
//   - `defaults` → callers construct their own `defaults []Rule` slice;
//                  built-in `build` / `plan` / `general` ship pre-merged
//                  results so a freshly-constructed Registry is usable.
//   - `user`     → s08's `cfg.Permissions` ([]Rule), passed as the
//                  `userConfig` arg of MergePermissions.
const defaults = Permission.fromConfig({
  "*": "allow",
  doom_loop: "ask",
  external_directory: { "*": "ask" /* + skill dir whitelisting */ },
  question: "deny",
  plan_enter: "deny",
  plan_exit: "deny",
  repo_clone: "deny",
  repo_overview: "deny",
  // mirrors github.com/github/gitignore Node.gitignore pattern for .env files
  read: { "*": "allow", "*.env": "ask", "*.env.*": "ask", "*.env.example": "allow" },
})
const user = Permission.fromConfig(cfg.permission ?? {})

// L122-L175 — the three built-ins our Go module ports. Each shows the
// same three-layer pattern: `Permission.merge(defaults, agentSpecific, user)`.
//
// Notice the order in upstream is (defaults, agentSpecific, user) — the
// AGENT's specific rules come BEFORE the user config. Our Go contract is
// the symmetric one: (defaults, user, agentOverride) with the agent LAST,
// matching the agent.ts L282-L304 override loop where a user-defined agent
// fully replaces the built-in. Both orders preserve the load-bearing
// guarantee — *agent has the final say* — but we put the agent layer last
// because the upstream `cfg.agent[name].permission` IS the agent's last
// word, layered on top of whatever the built-in default already provided.
const agents = {
  build: {
    name: "build",
    description: "The default agent. Executes tools based on configured permissions.",
    options: {},
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
    options: {},
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({
        question: "allow",
        plan_exit: "allow",
        external_directory: { /* plan dir whitelist */ },
        edit: { "*": "deny", /* + plan markdown whitelist */ },
      }),
      user,
    ),
    mode: "primary",
    native: true,
  },
  general: {
    name: "general",
    description: `General-purpose agent for researching complex questions and executing multi-step tasks. Use this agent to execute multiple units of work in parallel.`,
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({ todowrite: "deny" }),
      user,
    ),
    options: {},
    mode: "subagent",
    native: true,
  },
  // explore / scout / compaction / title / summary follow at L176-L275 —
  // out of s09's scope; the build / plan / general triple is enough to
  // show the cascade contract.
}

// L282-L304 — the override loop. For every agent block in `cfg.agent`,
// EITHER patch the existing entry OR install a new one. Note line 287:
// when no built-in matches the user's name, the new agent's permission is
// `Permission.merge(defaults, user)` — only TWO layers, since there's no
// agent-specific layer yet. Our Go equivalent: `Register(a)` replaces
// wholesale, so the caller is responsible for choosing whether to inherit
// (copy from a built-in then mutate) or start fresh (construct an Agent
// with `Permissions: MergePermissions(defaults, user, ownRules)`).
for (const [key, value] of Object.entries(cfg.agent ?? {})) {
  if (value.disable) {
    delete agents[key]
    continue
  }
  let item = agents[key]
  if (!item)
    item = agents[key] = {
      name: key,
      mode: "all",
      permission: Permission.merge(defaults, user),
      options: {},
      native: false,
    }
  if (value.model) item.model = Provider.parseModel(value.model)
  item.variant = value.variant ?? item.variant
  item.prompt = value.prompt ?? item.prompt
  // ...remaining patch-merge fields (description, temperature, topP, mode,
  // color, hidden, name, steps, options, permission)...
  item.permission = Permission.merge(
    item.permission,
    Permission.fromConfig(value.permission ?? {}),
  )
}

// END EXCERPT — the file continues with explore / scout / compaction /
// title / summary built-ins (L176-L275), the Truncate.GLOB whitelist
// post-pass (L307-L320), and the LLM-driven `generate(...)` agent
// synthesizer (L321-L460). All three regions are out of scope for s09's
// teaching contract; the schema + cascade + override loop above are the
// part we port.
