---
title: "s_full · End-to-end integration"
chapter: 99
slug: s_full-integration
est_read_min: 18
---

# s_full · End-to-end integration

> What this chapter teaches: there is no new code in s_full. Every mechanism it uses already exists in s01..s14 (with s12 MCP and s13 LSP marked TODO — coming in a later release). What is new is the **wiring**: how the 12 small modules compose into one coherent agent that can take `opencode "add error handling to app.ts"` from a CLI string to a SQLite-persisted, cost-tracked, permission-gated, multi-turn tool-using conversation. Read this when you want the bird's-eye view; read s01..s14 when you want to know how each box is built.

---

## Architecture overview

Twelve modules. One arrow per data dependency. Read top-to-bottom, left-to-right; bend back upward only where state is persisted.

```
                 +----------------+
                 |     CLI argv   |   "opencode 'add error handling to app.ts'"
                 +--------+-------+
                          |
                          v
   +----------+    +--------------+     +------------+    +-----------+
   |  Config  |--->| AgentRegistry|<----| Skills cat.|    |  Session  |
   |   s08    |    |     s09      |     |    s11     |    |    s07    |
   +----------+    +------+-------+     +------+-----+    +-----+-----+
       JSON merge        |                     |                ^
                         v                     v                |
                  +--------------+     system prompt       persist every
                  | Orchestrator |<----+                   Message + Part
                  |     s10      |     |                        |
                  +------+-------+     |                        |
                         |             |                        |
              Request    v       merge in skill catalog         |
                  +--------------+                              |
                  |   Provider   |   Stream(ctx, Request)       |
                  |     s05      |---+                          |
                  +------+-------+   |                          |
                         | SSE       v                          |
                         |     +-----------+                    |
                         +---->| Streaming |   Event chan       |
                               | Loop  s06 |---+                |
                               +-----------+   | parts          |
                                               v                |
                            +--------+   +----------+           |
                tool_use -->| Permis |-->| ToolReg  |--exec---->|
                            | s04    |   |   s03    |           |
                            +--------+   +----+-----+           |
                                              | result          |
                                              v                 |
                                         +----------+           |
                                         |  Usage   |           |
                                         |   s14    |---accum-->|
                                         +----------+           |
                                                                |
                  +-----------+        retry on 429,            |
                  |  Retry    |<-------classify auth/overflow---+
                  |   s14     |
                  +-----------+
```

What the diagram is and is not saying:

- **Solid arrows = function calls**. Config feeds AgentRegistry once at boot; AgentRegistry feeds Orchestrator the named Agent (with model, system prompt, permission ruleset). Orchestrator drives the Provider; Provider streams Events back into the Streaming Loop; the Loop emits PartToolUse values to the Tool Registry, gated by Permission. Each round-trip writes to the Session store and accumulates into Usage.
- **No box for the LLM itself** — it's the cloud on the other side of Provider. From learn-opencode's perspective Anthropic's Messages API is one HTTP POST and one SSE response, period.
- **No box for s01 or s02** — those are bootstrap chapters whose code is subsumed: s01's "make one HTTP call" lives inside s05's `AnthropicProvider`; s02's `Part`/`Message` types are reproduced in every later session because each session is its own Go module with no cross-session imports.
- **MCP (s12) and LSP (s13)** would each plug into the Tool Registry as remote tools. They are TODO — coming in a later release; the wire shape (s03's `Tool` interface) is already designed to absorb them without orchestrator changes.

## End-to-end use case

The use case from research-notes.md A3, retold step-by-step with both upstream and learn-opencode citations.

User invocation:

```bash
opencode "add error handling to app.ts"
```

The 16-step trace below cites paths relative to `.learn/upstream/`. Each step also names the learn-opencode session that re-implements it; "—" means the step is TUI/wrapper plumbing learn-opencode deliberately omits.

| # | What happens | Upstream file | learn-opencode |
|---|---|---|---|
| 1 | Node wrapper detects platform, spawns prebuilt binary. | `packages/opencode/bin/opencode` | — (Go is one binary) |
| 2 | Yargs parses argv; loads `Global.Path`; sets `OPENCODE_PID`. | `packages/opencode/src/index.ts` L58–L120 | s01 `main.go` (single-file CLI) |
| 3 | `RunCommand` dispatch hands off to runtime task. | `packages/opencode/src/cli/cmd/run.ts` L1–L40 | s10 `Orchestrator.Run` entry |
| 4 | Runtime resolves stdin/piped input; instantiates `OpencodeClient`; creates Session. | `packages/opencode/src/cli/cmd/run/runtime.ts` | s07 `OpenDB` + `CreateSession` |
| 5 | `Session.Service.create()` allocates SessionID, writes SessionTable row. | `packages/opencode/src/v2/session.ts` L68–L75 | s07 `db.go` (`Session`/`Message`/`Part` tables) |
| 6 | Prompt submitted: `client.session.prompt({ text, model, agent: "build" })`. | (same file as #5) | s10 `Orchestrator.Run(ctx, Message{Role: User})` |
| 7 | Agent lookup: `Agent.Service.get("build")` merges permissions, picks model. | `packages/opencode/src/agent/agent.ts` L58–L70 | s09 `AgentRegistry.Resolve("build")` |
| 8 | Config (s08) was already loaded at boot; agent permissions are the cascade defaults→config→agent override. | `packages/opencode/src/config/config.ts` L49–L110 + `agent.ts` L128–L304 | s08 `Config.Load` + s09 `mergePermissions` |
| 9 | Skills are scanned from `.opencode/skills/` and `~/.opencode/skills/`; a catalog string is appended to the system prompt. | `packages/opencode/src/skill/index.ts` L36–L150 | s11 `Discover` + `Catalog.String()` |
| 10 | LLM invocation: `streamText({ model, system, messages, tools })`. SSE response begins. | `packages/opencode/src/session/llm.ts` L35–L120 | s05 `AnthropicProvider.Stream(ctx, Request)` |
| 11 | Streaming loop consumes `content_block_delta` / `tool_use_start` / `message_stop` events, building Parts. | `packages/opencode/src/session/llm.ts` L100–L200 | s06 `Loop.Consume(stream)` |
| 12 | First tool_use arrives (e.g. `read("app.ts")`). Permission evaluator checks `read:app.ts` against the merged ruleset → `allow`. | `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 `Evaluate(perm, target, rulesets...)` |
| 13 | Tool dispatch: `Registry.Dispatch("read", input)` returns the file body; result becomes a `PartToolResult`. | `packages/opencode/src/tool/registry.ts` L56–L150 + `tool/tool.ts` L35–L77 | s03 `Registry.Dispatch` + s10 result feedback |
| 14 | Orchestrator packs all results into a user Message and re-calls Provider. Loop until assistant emits `end_turn` (or `MaxIterations`). | `packages/opencode/src/session/processor.ts` L34–L150 | s10 `Orchestrator.Run` main loop |
| 15 | After each request, `Usage{InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens}` is summed onto the SessionRow. Errors flow through retry classifier (429 backoff, 401 bail, context-overflow → compaction signal). | `packages/opencode/src/session/session.ts` L91–L142 + `session/message-error.ts` | s14 `Usage.Add` + `WithRetry` + four error types |
| 16 | Idle timeout fires; final messages flushed; SQLite closed; exit 0. | `packages/opencode/src/cli/cmd/run.ts` finalize | s07 `db.Close()` (deferred) |

What the trace is showing:

- **There is exactly one agent loop** — it lives in s10's `Orchestrator.Run`. Steps 10–14 happen in a single Go function body, with the SSE stream from s05/s06 feeding tool calls to s03 gated by s04, results going back into the next request body.
- **Two side channels are not in the loop body**: persistence (every message+part also lands in s07's SQLite) and usage accounting (every `EventFinish` adds to s14's `Usage`).
- **One scope expansion is not shown**: when the conversation crosses ~120k tokens the Provider returns `context_length_exceeded`, s14's classifier returns `ShouldCompact == true`, and the orchestrator would call a compaction routine (out of scope for s14 — the signal exists, the implementation is left as exercise).

## Module map

Twelve rows, one per upstream→learn-opencode mapping. Use this when you have an upstream file open and want to find the matching Go.

| Upstream file | Session | Key files in learn-opencode |
|---|---|---|
| `packages/opencode/src/session/llm.ts` L35–L120 | s01 `minimum-loop` | `agents/s01-minimum-loop/main.go`, `provider.go` |
| `packages/opencode/src/session/message-v2.ts` L1–L150 | s02 `message-parts` | `agents/s02-message-parts/parts.go` |
| `packages/opencode/src/tool/registry.ts` L56–L150 + `tool/tool.ts` L35–L77 | s03 `tool-registry` | `agents/s03-tool-registry/tool.go`, `builtin_*.go` |
| `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 `permission-eval` | `agents/s04-permission-eval/permission.go` |
| `packages/opencode/src/provider/provider.ts` L87–L150 | s05 `provider-iface` | `agents/s05-provider-iface/provider.go`, `stream.go` |
| `packages/opencode/src/session/llm.ts` L100–L200 (SSE framing) | s06 `streaming-loop` | `agents/s06-streaming-loop/sse.go`, `loop.go`, `fake_provider.go` |
| `packages/opencode/src/session/session.ts` L1–L110 + `session.sql.ts` | s07 `session-store` | `agents/s07-session-store/db.go`, `schema.go` |
| `packages/opencode/src/config/config.ts` L49–L110 + `config/paths.ts` | s08 `config-load` | `agents/s08-config-load/config.go`, `paths.go` |
| `packages/opencode/src/agent/agent.ts` L28–L304 | s09 `agent-registry` | `agents/s09-agent-registry/agent.go`, `builtin_agents.go` |
| `packages/opencode/src/session/processor.ts` L34–L150 | s10 `tool-loop` | `agents/s10-tool-loop/loop.go` |
| `packages/opencode/src/skill/index.ts` L36–L150 | s11 `skills` | `agents/s11-skills/skill.go` |
| `packages/opencode/src/session/session.ts` L91–L142 + `session/message-error.ts` | s14 `cost-and-recovery` | `agents/s14-cost-and-recovery/usage.go`, `retry.go`, `compaction.go` |

Two TODOs — coming in a later release:

- `packages/opencode/src/config/mcp.ts` + `packages/opencode/src/llm/protocols/mcp.ts` → s12 `mcp-client` (spawn child + JSON-RPC over stdio; remote tools register into s03's `Registry`).
- `packages/opencode/src/lsp/lsp.ts` L17–L100 → s13 `lsp-client` (gopls subprocess + workspace symbols; LSP-shaped tools register into s03's `Registry`).

When s12 and s13 ship, they slot in at step 13 of the use-case trace (Tool dispatch) without orchestrator changes — that's the load-bearing benefit of s03's `Tool` interface being source-of-truth for "what can be dispatched".

## Deliberate omissions

opencode is a 183K-LOC TypeScript application. learn-opencode is a teaching repo deliberately under 6K LOC. The list below is what we left out and why; if you want to dig further on any row, the upstream file is the place to start.

| Feature | Why omitted | Upstream file (for the curious) |
|---|---|---|
| TUI rendering (OpenTUI / Solid.js, scrollback + footer split) | A reactive Solid-on-terminal renderer with reflow and prompt UI is a multi-thousand-LOC project on its own; orthogonal to the agent loop. Headless CLI is enough to teach the LLM mechanics. | `packages/opencode/src/cli/cmd/run/scrollback.surface.ts`, `packages/opencode/src/cli/cmd/run/footer.permission.tsx` |
| MCP client (spawn server, JSON-RPC handshake, remote tool surfacing) | TODO — coming in a later release as s12. The hook point in s03 is already shaped for it. | `packages/opencode/src/config/mcp.ts`, `packages/opencode/src/llm/protocols/mcp.ts` |
| LSP client (workspace symbols, definition, hover) | TODO — coming in a later release as s13. Same JSON-RPC machinery as s12 with LSP method names. | `packages/opencode/src/lsp/lsp.ts` |
| Bun-specific FFI (`Bun.$`, `Bun.file`, `Bun.SQLite`) | Bun isn't portable to a Go teaching repo; we use `os/exec`, `os.ReadFile`, `mattn/go-sqlite3`. | `packages/opencode/src/storage/db.bun.ts` |
| Effect-based DI (`Layer`, `Context`, `Service`, `Effect.gen()`) | Go uses constructor injection + `context.Context`; replicating Effect's typed-error machinery would obscure the agent mechanics. | `packages/opencode/src/effect/` |
| Drizzle ORM with migration runner | Idempotent `CREATE TABLE IF NOT EXISTS` covers a teaching schema; production migrations need a real story we're not teaching. | `packages/opencode/src/storage/db.bun.ts` (drizzle-kit usage) |
| Sharing / GitHub PR export | OAuth + Octokit + share API surface is a feature, not a mechanism. | `packages/opencode/src/share/`, `packages/opencode/src/sync/` |
| Mobile / ACP server (remote agent control protocol) | A second wire format (ACP) on top of the agent loop; the loop itself is what we're teaching. | `packages/opencode/src/acp/` |
| Plugin loader with hot reload (chokidar-watched) | Build-tag plugins or constructor injection cover the same ground in Go without a watcher subsystem. | `packages/opencode/src/plugin/index.ts` L59–L150 |
| Multi-process (CLI + headless server + SDK client) | Single-process Go binary is simpler to read and run. | `packages/opencode/src/cli/cmd/serve.ts`, `packages/opencode/src/v2/` |
| GitHub Copilot / Codex / Azure / DigitalOcean auth providers | Each is a one-off OAuth flow; the BUNDLED_PROVIDERS map (Appendix A) is the mechanism, not the individual entries. | `packages/opencode/src/plugin/github-copilot/copilot.ts`, `packages/opencode/src/auth/` |
| Snapshot / git-diff revert on tool failure | Belongs in a future "safety nets" chapter; out of scope for the core loop. | `packages/opencode/src/snapshot/` |

## What to read next

Four reading paths through learn-opencode, depending on what you came for.

**Path A — the through-line (the canonical order)**

Read s01 → s02 → s03 → s04 → s05 → s06 → s07 → s08 → s09 → s10 → s11 → s14 → s_full. Each chapter assumes everything before it. This is the order the curriculum is designed around: by the end of s10 you've built a real loop, and s11 / s14 / s_full extend it.

**Path B — thematic by area**

If you only care about one slice of opencode:

- **LLM transport**: s01 (one-shot HTTP) → s05 (Provider interface + Anthropic) → s06 (streaming SSE).
- **Tools and safety**: s03 (Registry) → s04 (Permissions) → s10 (gated dispatch in the loop) → Appendix A (why the abstractions are interfaces).
- **Persistence and accounting**: s07 (SQLite tables) → s14 (Usage + retry classifier).
- **User-facing configuration**: s08 (opencode.json hierarchy) → s09 (Agent registry) → s11 (Skills).

**Path C — start from the loop**

If you've read agent papers and want to see "the loop" first, start in **s10** (`agents/s10-tool-loop/loop.go`). When you hit the `provider.Stream(ctx, req)` call, jump back to s06 (consuming the stream) and s05 (where the stream comes from). When you hit `permission.Evaluate(...)`, jump to s04. When you hit `registry.Dispatch(...)`, jump to s03. The loop is ~250 LOC; everything it depends on is one chapter away.

**Path D — the "agentic" path**

If you came specifically for the agent abstractions (multi-agent systems, permission cascades, cost ceilings), read s09 → s10 → s14 in that order. s09 explains how an Agent is defined and how its permission ruleset is the merge of three sources; s10 shows how that ruleset gates each tool call inside the loop; s14 shows how the loop's cost is observed and how its failures are classified for recovery. After this, jump to Appendix A for the "why interface, not concrete type" mental model.

## Where to go after learn-opencode

When you've read all 12 chapters and want to keep going:

- **Build s12 and s13.** The interfaces are designed for them. s12 needs `os/exec` + a tiny JSON-RPC client over stdio + a `MCPTool` adapter that implements `Tool`. s13 is the same machinery talking to `gopls`. ~600 + ~500 LOC respectively.
- **Replace the headless CLI with a real TUI.** A `bubbletea`/`tcell` Go port of opencode's scrollback+footer would be a healthy 2K-LOC sub-project, completely orthogonal to anything in s01..s14.
- **Add a second provider.** Appendix A walks through what `OpenAIProvider` would need: same `Provider` interface, different request shape, different SSE framing. The point of s05's interface is that nothing else in the agent has to change.
- **Read the upstream.** With all 12 sessions in your head, opencode's 183K-LOC core CLI package becomes navigable. Appendix B is the reading map; the most rewarding next file is `packages/opencode/src/session/processor.ts` — the full version of s10's `Orchestrator.Run`, with snapshot-on-tool-call, doom-loop detection, and the real compaction trigger.
