---
title: "Appendix B · Upstream source-reading map"
chapter: 101
slug: appendix-b-upstream-map
est_read_min: 22
---

# Appendix B · Upstream source-reading map

> What this appendix teaches: how to navigate the 183K-LOC `packages/opencode/src/` tree with learn-opencode as your map. Four reading paths through the upstream source; a 30-row file→session table for when you have an upstream file open and want the matching learn-opencode chapter; ~30 extension exercises across the 12 sessions for when you want to grow the teaching code in interesting directions; the 18-package monorepo's in-scope / out-of-scope split; and 7 adjacent projects worth knowing about for context. Use this as a reference, not a sit-down read.

---

## Reading order

opencode is too big to read top-down. Pick one of these four paths depending on what you came for.

**Path (a) — chronologically by feature complexity**

Start with the smallest mechanism and add one per file. This mirrors the learn-opencode curriculum but in upstream coordinates:

1. `packages/opencode/src/permission/evaluate.ts` (~50 LOC) — pure function, no deps. Read the wildcard pattern, the `findLast` semantic, the three actions.
2. `packages/opencode/src/session/message-v2.ts` (~150 LOC of part definitions) — the data model. Skim Zod schemas; what matters is the Part union shape.
3. `packages/opencode/src/tool/tool.ts` + `packages/opencode/src/tool/registry.ts` — the Tool interface (think Go interface) and the registry that dispatches by name.
4. `packages/opencode/src/provider/provider.ts` (1792 LOC, but only the BUNDLED_PROVIDERS map and the `model()` function matter for now).
5. `packages/opencode/src/session/llm.ts` — `streamText` invocation. The orchestration loop is here, in stripped-down form.
6. `packages/opencode/src/session/session.ts` + `session.sql.ts` — persistence layer.
7. `packages/opencode/src/agent/agent.ts` — agent definitions and the permission cascade.
8. `packages/opencode/src/session/processor.ts` — the full tool execution loop with snapshot, doom-loop detection, and compaction signaling.
9. `packages/opencode/src/lsp/lsp.ts` and `packages/opencode/src/llm/protocols/mcp.ts` — the two stdio-RPC integrations.

**Path (b) — by user-facing flow**

Follow what happens when a user types `opencode "..."` at the shell:

1. `packages/opencode/bin/opencode` (Node wrapper).
2. `packages/opencode/src/index.ts` (Yargs entry).
3. `packages/opencode/src/cli/cmd/run.ts` (RunCommand).
4. `packages/opencode/src/cli/cmd/run/runtime.ts` (input plumbing + Session creation).
5. `packages/opencode/src/v2/session.ts` (`Session.Service.create` / `prompt`).
6. `packages/opencode/src/agent/agent.ts` (`Agent.Service.get`).
7. `packages/opencode/src/session/llm.ts` (`streamText` invocation).
8. `packages/opencode/src/session/processor.ts` (streaming + tool dispatch loop).
9. `packages/opencode/src/permission/index.ts` (per-tool gating).
10. `packages/opencode/src/cli/cmd/run/scrollback.surface.ts` (terminal rendering).
11. `packages/opencode/src/cli/cmd/run/footer.permission.tsx` (interactive permission prompt).

**Path (c) — by package**

The `packages/opencode/src/` tree has ~30 sub-packages. The load-bearing ones, in dependency order:

`config/` → `auth/` → `provider/` → `tool/` → `permission/` → `agent/` → `session/` (+`storage/`) → `cli/cmd/run/` → `mcp/` and `lsp/`

Plus orthogonal: `bus/` (event bus), `effect/` (DI machinery), `plugin/` (extension hooks), `skill/` (SKILL.md discovery), `snapshot/` (git-diff revert).

**Path (d) — if you only have 1 hour**

Read these three files in this order, nothing else:

1. `packages/opencode/src/permission/evaluate.ts` (10 minutes) — gives you the entire mental model of how opencode says "no" to a tool call.
2. `packages/opencode/src/session/processor.ts` L34–L150 (30 minutes) — the actual agent loop. Read until you can sketch on a napkin: pull stream → on tool_use, gate → execute → re-call.
3. `packages/opencode/src/agent/agent.ts` L28–L304 (20 minutes) — what an Agent is, how its permissions are merged, why "build", "plan", "general" are different things.

After this hour, you can answer "what is opencode" without hand-waving. Everything else is implementation choice or polish.

## Per-file → which session(s) reference it

When you have an upstream file open and want to know which learn-opencode chapter teaches its mechanism, look it up here. Sorted by upstream path. ≥30 rows.

| Upstream file | Session(s) | First time you should read it |
|---|---|---|
| `packages/opencode/bin/opencode` | (none — bootstrap shell script) | When you want to see how the Node wrapper picks the platform binary. |
| `packages/opencode/package.json` L74–L169 | (none — dependency map) | Right after Quickstart; tells you which AI SDK provider packages are bundled. |
| `packages/opencode/src/index.ts` L58–L120 | s01 (CLI entry analog) | When tracing the boot path; shows `Global.Path` setup. |
| `packages/opencode/src/cli/cmd/run.ts` L1–L40 | s10 | When you want to see the user-input → orchestrator handoff. |
| `packages/opencode/src/cli/cmd/run/runtime.ts` | s07 (Session create), s10 | When you want to see stdin/piped input handling and Session bootstrap. |
| `packages/opencode/src/cli/cmd/run/demo.ts` | (out of scope) | When you want to study the codebase without API keys — synthetic event source. |
| `packages/opencode/src/cli/cmd/run/scrollback.surface.ts` | (out of scope — TUI) | If you ever attempt a Go TUI port. |
| `packages/opencode/src/cli/cmd/run/footer.permission.tsx` | s04 (the prompt UI side; eval logic is shared) | When studying the human-in-the-loop side of permission asks. |
| `packages/opencode/src/cli/cmd/run/stream.ts` | s06 + s10 (event reduction → state machine) | When you want to see how SSE events become UI state. |
| `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 | Path (d) item #1 — read first. |
| `packages/opencode/src/permission/index.ts` L14–L29 | s04 + s10 (ask-event emission) | After s04, when you want to see how "ask" actions reach the user. |
| `packages/opencode/src/config/config.ts` L49–L110 | s08 | When you read s08; opencode's full schema is here. |
| `packages/opencode/src/config/paths.ts` | s08 | Right after `config.ts`; shows the upward-walk for `.opencode/`. |
| `packages/opencode/src/config/mcp.ts` | s12 (TODO — coming in a later release) | Skim now; come back when s12 ships. |
| `packages/opencode/src/auth/index.ts` | (out of scope) | When you want to add a non-API-key provider (OAuth, Bedrock IAM). |
| `packages/opencode/src/provider/provider.ts` L87–L150 | s05, Appendix A | After s05; this is the BUNDLED_PROVIDERS map. |
| `packages/opencode/src/provider/provider.ts` (rest of 1792 LOC) | (reference) | When debugging a specific provider integration. |
| `packages/opencode/src/session/message-v2.ts` L1–L150 | s02 | Right after s02 to see opencode's full Part union. |
| `packages/opencode/src/session/session.ts` L1–L110 | s07 | After s07 to see the full schema. |
| `packages/opencode/src/session/session.ts` L91–L142 | s14 | When studying cost tracking. |
| `packages/opencode/src/session/session.sql.ts` | s07 | Side-by-side with `session.ts` for the SQL definitions. |
| `packages/opencode/src/session/llm.ts` L35–L120 | s01 (one-shot) → s05 (provider) → s06 (streaming) | s01 when first reading; reread after s06 to see the full streaming flow. |
| `packages/opencode/src/session/llm.ts` L100–L200 | s06 | The SSE consumer side; read after s06. |
| `packages/opencode/src/session/processor.ts` L34–L150 | s10 | Path (d) item #2 — read second. |
| `packages/opencode/src/session/processor.ts` L734–L802 | s10 (max-iterations / doom-loop) | After s10 when you want to see the safety nets. |
| `packages/opencode/src/session/message-error.ts` | s14 | When studying error classification. |
| `packages/opencode/src/tool/registry.ts` L56–L150 | s03 | Right after s03 to see the full tool catalog. |
| `packages/opencode/src/tool/tool.ts` L35–L77 | s03 | Side-by-side with the registry; defines the Tool interface. |
| `packages/opencode/src/tool/edit.ts` (and other built-ins) | s03 (shape only — the actual `edit` is ~600 LOC) | When implementing your own real edit tool. |
| `packages/opencode/src/agent/agent.ts` L28–L70 | s09 | Read after s08; built-in agents and their resolution. |
| `packages/opencode/src/agent/agent.ts` L128–L304 | s09 | The permission cascade; the most subtle part of agent.ts. |
| `packages/opencode/src/skill/index.ts` L36–L150 | s11 | After s11 for the full discovery flow including caching. |
| `packages/opencode/src/lsp/lsp.ts` L17–L100 | s13 (TODO — coming in a later release) | Skim now; come back when s13 ships. |
| `packages/opencode/src/llm/protocols/mcp.ts` | s12 (TODO — coming in a later release) | Skim now; come back when s12 ships. |
| `packages/opencode/src/storage/db.bun.ts` | s07 (the Bun-specific equivalent we replace with `mattn/go-sqlite3`) | When you want to see opencode's drizzle-orm usage. |
| `packages/opencode/src/plugin/index.ts` L59–L150 | (out of scope) | When you want to add an external plugin to your own fork. |
| `packages/opencode/src/snapshot/` | (out of scope) | When you want to add tool-failure rollback to learn-opencode. |
| `packages/opencode/src/v2/session.ts` L68–L80 | s07 + s10 (the v2 client API; learn-opencode embeds the same shape directly) | When studying multi-process opencode. |

## Suggested extension exercises

If learn-opencode taught you the bones, these exercises grow the muscle. Pick the ones that match what you want to build next.

### s01 extensions:

- Switch the request body to *streaming* (`stream: true`) and consume the SSE without using s06's framework — just to feel the wire format yourself.
- Add a `--model` flag that lets you swap `claude-3-5-sonnet` for `claude-3-5-haiku`; observe the cost difference.
- Add a `--system` flag that loads from a file; see how a longer system prompt changes latency.

### s02 extensions:

- Add a `Part.Marshal` / `Part.Unmarshal` benchmark; tagged unions in Go are not free, measure the cost.
- Add a `PartImage` variant (Anthropic supports image inputs); roundtrip a base64-encoded PNG.
- Write a `String()` method on `Part` that renders it terminal-friendly (colorized text vs `[tool: read]` vs `[result: 14 lines]`).

### s03 extensions:

- Implement a real `tree` tool that walks `os.ReadDir` to a configurable depth; pay attention to the `JSONSchema()` for `max_depth: integer`.
- Add a `Registry.Disable(name string)` method; thread it into a config-driven "tools blocklist".
- Implement a `RegistryCatalog()` function that emits a markdown table of tools — useful for `--list-tools` CLI.

### s04 extensions:

- Add a `permission deny:bash:rm -rf*` test case; demonstrate that "deny later overrides allow earlier".
- Add a CIDR-style pattern (`edit:src/**`) by wrapping `filepath.Match` with `**` expansion.
- Add a `Permission.Explain(target string)` that walks the ruleset and tells you which rule matched (auditable permissions).

### s05 extensions:

- Implement `OpenAIProvider` (Anthropic was the easy one — OpenAI uses a slightly different SSE framing and tool shape; both translate into the canonical Event).
- Add a `BedrockProvider` that signs requests with AWS sigv4.
- Add a `RecordingProvider` that wraps another Provider and writes every (request, response) to a JSONL file for replay tests.

### s06 extensions:

- Add a `--debug-events` flag that dumps every parsed Event to stderr; great for understanding any new provider's streaming format.
- Add `Loop.OnEvent(func(Event))` callback so the TUI can render incrementally.
- Inject random network errors mid-stream and verify the error classification (s14) still works end-to-end.

### s07 extensions:

- Add a `db.SearchMessages(query string)` using SQLite FTS5; opencode doesn't ship this and it's an instructive ~100 LOC.
- Migrate the schema from `mattn/go-sqlite3` to `modernc.org/sqlite` (pure-Go); compare binary size and `cgo` constraints.
- Add a `db.Vacuum()` and a `db.Backup(path)` for production readiness.

### s08 extensions:

- Add JSONC support (`//` and `/* */` comments) by writing a comment stripper before `encoding/json`; opencode does this with a third-party lib.
- Add an `OPENCODE_CONFIG_OVERRIDE` env var that takes a JSON string and merges last; useful for CI.
- Add config schema validation: emit a friendly error pointing at the offending line when `provider.modelID` is missing.

### s09 extensions:

- Implement a custom "researcher" agent: read-only permissions, model defaulted to opus, system prompt biased toward citing sources.
- Add an `AgentRegistry.List(mode Mode)` filter so the CLI can list only `ModePrimary` agents.
- Add agent-level `Tools []string` whitelist enforcement (currently only permissions enforce; tool list is advisory).

### s10 extensions:

- Add a `--max-iterations` flag and a test that verifies the cap actually fires (not just plumbed through).
- Add a `--dry-run` mode that gates every tool through `ask` and never executes; useful for previewing what an agent would do.
- Add the missing `compaction` step: when s14 signals overflow, summarize the first half of messages into a single synthetic system message and retry.

### s11 extensions:

- Add per-skill capability declarations (`required_tools: [edit, bash]`) and warn at load time if the active agent doesn't permit them.
- Add a `SKILL.md` linter that catches missing frontmatter fields.
- Add hot reload: re-scan the skills directory on each `Run()` call (cheap; ~5ms for 50 skills).

### s14 extensions:

- Implement the actual compaction routine signaled by `ContextOverflowError` — summarize messages 0..N-10 with a side LLM call.
- Add `pricing.go` that loads rates from a JSON file at boot instead of hardcoded constants.
- Add a `--budget-cents` CLI flag that aborts the loop when `Usage.TotalCost(p) * 100 > budget`.

## The 18 packages opencode ships

opencode is a monorepo with packages under `packages/`. The split between in-scope and out-of-scope for learn-opencode reflects "is this teaching the agent loop, or teaching infrastructure around it?"

| Package | Scope | One-line reason |
|---|---|---|
| `packages/opencode/` | **in scope** | The core CLI; everything s01..s14 reimplements is here. |
| `packages/sdk/` | out of scope | TypeScript client for opencode's HTTP server; learn-opencode is single-process. |
| `packages/plugin/` | out of scope | Plugin SDK for npm-distributed plugins; Go has build-tag plugins instead. |
| `packages/console/` | out of scope | Web console for hosted opencode; not needed for local agent. |
| `packages/web/` | out of scope | Marketing site (also not the doc viewer). |
| `packages/docs/` | out of scope | Astro-built docs site (separate from learn-opencode's `docs/`). |
| `packages/desktop/` | out of scope | Electron wrapper; trades on the same CLI. |
| `packages/app/` | out of scope | Mobile/PWA frontend that talks to opencode's ACP server. |
| `packages/extensions/` | out of scope | Editor extensions (VS Code, JetBrains). |
| `packages/storybook/` | out of scope | UI component playground. |
| `packages/ui/` | out of scope | Shared UI primitives for the desktop/web/console. |
| `packages/identity/` | out of scope | OAuth identity provider for hosted opencode. |
| `packages/share/` | out of scope | Sharing via signed URLs (mentioned in s_full omissions). |
| `packages/slack/` | out of scope | Slack integration for hosted opencode. |
| `packages/script/` | out of scope | Build/release scripts; not part of the runtime. |
| `packages/http-recorder/` | out of scope | Test fixture recorder; learn-opencode uses `httptest` instead. |
| `packages/containers/` | out of scope | Docker manifests for hosted opencode. |
| `packages/enterprise/` | out of scope | License-gated enterprise features. |

The 17 out-of-scope packages add up to 200K+ LOC. The decision to ignore them is the single biggest reason learn-opencode is readable in a weekend.

## Adjacent reading

Six neighbors of opencode worth knowing about. None are dependencies; reading them gives you context on what design choices opencode made and what the alternatives are.

| Project | Why interesting |
|---|---|
| **Claude Code** (Anthropic's official CLI) | The closest analog. Closed-source, but the public docs and the [Claude Code SDK](https://docs.anthropic.com/en/docs/claude-code/sdk) describe a similar agent loop with tools, permissions, and sessions. Compare the *user surface*; opencode is a re-implementation philosophy ("if Claude Code closed, would the workflow survive?"). |
| **sst/sst** (same author, infra-as-code) | opencode lives in the SST org. The Effect-based DI, the local-first state, the npm-monorepo with Bun build setup — all SST house style. Reading SST first explains why opencode looks the way it does. |
| **Vercel AI SDK** (`ai` on npm) | The library opencode wraps. The interface design lessons (LanguageModelV3, providerOptions, tool format normalization) are the basis of Appendix A. Even if you never write TS, the design is portable. |
| **Continue** (continue.dev) | An open IDE-extension agent. Different surface (in-editor sidebar, not CLI), same loop. Their `Tool` model and `provider` adapters make a useful comparison; they ship an MCP integration in production. |
| **Cursor** (closed-source, but observable) | The product opencode positions against. Different bet: deeply integrated editor vs. terminal-native. Watching Cursor's tool roster evolve (search → grep → edit → background agents) is a roadmap preview for any agent. |
| **Aider** (paul-gauthier/aider) | A Python agent that predates the current wave. Smaller surface, no permission system, no MCP — but a clear reference for "what's the minimum viable coding agent". Reading it after learn-opencode shows what opencode adds. |
| **Anthropic's `mcp` reference impls** ([modelcontextprotocol/servers](https://github.com/modelcontextprotocol/servers)) | The MCP servers opencode would talk to. When s12 ships, this is where you'll find example servers (filesystem, git, postgres, etc.) to test against without spinning up your own. |
