---
title: "s10 · Tool execution loop"
chapter: 10
slug: s10-tool-loop
est_read_min: 13
---

# s10 · Tool execution loop

> What this chapter teaches: assemble all the pieces from the previous nine chapters — Provider (s05/s06) + Tool Registry (s03) + Permission Evaluate (s04) + Message/Part (s02) — into an actual agent loop. `Orchestrator.Run(ctx, initial)` walks a 5-step iteration: pull one Stream → assemble one assistant Message → find every tool_use inside → run each one (permission gate + tool execute) → pack all results into a single user Message → feed back to the LLM. Repeat until the assistant emits no more tool calls (natural end_turn) or hits `MaxIterations`. Mirrors upstream `packages/opencode/src/session/processor.ts` L34-L150 + L734-L802.

---

## Problem

By s09, every individual piece works: we can talk to the LLM (s05/s06), invoke tools (s03), evaluate permissions (s04), break messages into Parts (s02), load configs and agents (s08/s09). But **they've never been wired together**.

Concretely: s06's `Loop.Consume` drains one stream and stops — it gathers EventToolUse into PartToolUse, but no one *runs* the tool. s03's `Registry.Dispatch` can run a tool, but it doesn't know which tool to run — no one *scans* the assistant Message for the tool_use list. s04's `Evaluate` can decide whether a (permission, target) is allow/deny/ask, but it never gets called automatically — no one *gates* before tool execution.

The deeper problem is **multi-turn**. A real agent session looks like:

```
user:      change every TODO in a.go to FIXME
assistant: OK, let me grep for TODO first  +  tool_use(grep, "TODO", "a.go")
[run grep, get back 3 matches]
user:      tool_result(grep): a.go:5: // TODO ...
                              a.go:12: // TODO ...
                              a.go:38: // TODO ...
assistant: I'll edit them one at a time  +  tool_use(edit, ...)  +  tool_use(edit, ...)  +  tool_use(edit, ...)
[run 3 edits]
user:      tool_result(edit#1): ok
           tool_result(edit#2): ok
           tool_result(edit#3): ok
assistant: Done — replaced 3 TODOs with FIXME.
```

The LLM can only decide based on what it knows *up to the current turn*. Turn 1, it doesn't know where the TODOs are — it has to grep first; once grep results come back, turn 2 can decide what to edit; once edits come back, turn 3 can declare done. **Without a wrapper that strings these turns together, the agent is just a chatbot**.

s10's `Orchestrator` is that wrapper.

## Solution

One struct, one method:

```go
type Orchestrator struct {
    Provider      Provider
    Tools         *Registry
    Permissions   Ruleset    // already-merged cascade (s09's MergePermissions output)
    MaxIterations int        // safety cap; 0 = unlimited (production replaces with token-budget)
}

func (o *Orchestrator) Run(ctx context.Context, initial []Message) ([]Message, error)
```

Inside `Run` is the 5-step loop:

```
for iter := 0; iter < MaxIterations; iter++ {
    (1) req := build Request from running []Message
    (2) stream := Provider.Stream(ctx, req)
        assistant := drain stream into one Message
        append assistant to trail
    (3) if assistant has no tool_use Parts → return trail (natural end_turn)
    (4) for each tool_use in assistant.Parts:
          action := Permission.Evaluate(name, target, Permissions)
          if Deny  → tool_result{IsError: true, "permission denied: ..."}
          else     → out, err := Registry.Lookup(name).Execute(ctx, input)
                     err  → tool_result{IsError: true, err.Error()}
                     ok   → tool_result{IsError: false, out}
    (5) append user Message containing all tool_result Parts
}
return trail, ErrMaxIterationsExceeded  // cap hit
```

| Step | Uses which chapter's output | Key decision |
|---|---|---|
| (1) build Request | s06's ProviderMessage shape | `messagesToProvider` translates in-memory Parts → wire-shape ContentBlocks |
| (2) Stream + drain | s06's Loop.Consume algorithm | Copied into `consumeOne` (each chapter self-contained, no cross-imports) |
| (3) end_turn detection | s06's `inferStopReason` | Simple: if assistant Parts has no PartToolUse → done |
| (4a) Permission gate | s04's Evaluate | Last-match-wins; deny is NOT a Run-level error, it's in-band feedback |
| (4b) Tool dispatch | s03's Registry.Lookup + Tool.Execute | Errors are also in-band; `runOneTool` flattens all 3 outcomes into `*ToolResultPart` |
| (5) tool_results pack | s02's PartToolResult Part type | **One user Message, multiple tool_result Parts** (Anthropic's wire convention) |

**Two in-band decisions are especially load-bearing**:

- **Permission deny is in-band**: a denied tool produces `ToolResultPart{IsError:true, "permission denied"}`; the next iteration the LLM sees this denial. The LLM decides what to do — typically end the turn or try a different approach. Run does **not** return an error.
- **Tool errors are also in-band**: `Tool.Execute` returning err similarly becomes `ToolResultPart{IsError:true, err.Error()}`. The LLM reads its own error and self-corrects.

Run only returns a non-nil err in two cases: (a) `Provider.Stream` itself fails (transport-level), (b) MaxIterations cap fires. Everything else — deny, tool err, unknown tool — is an in-band signal to the LLM, loop continues.

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│  s10 Orchestrator.Run iteration shape                          │
│                                                                 │
│   trail = [user("...")]                                         │
│                                                                 │
│   ┌─ iter 0 ──────────────────────────────────────────────────┐ │
│   │ (1) req := messagesToProvider(trail) + toolSchemas()      │ │
│   │ (2) stream := Provider.Stream(ctx, req)                   │ │
│   │     → drain Events: text + text + tool_use(echo, "hi")    │ │
│   │     → assistant = Message{Parts: [text, tool_use]}        │ │
│   │     → append to trail                                     │ │
│   │ (3) collectToolUses(assistant.Parts) → [echo]             │ │
│   │     has tool_use → don't exit                             │ │
│   │ (4) for echo: Evaluate("echo", "{...}", Permissions)      │ │
│   │       → ActionAllow → tool.Execute → "hi"                 │ │
│   │       → tool_result{IsError:false, Content:"hi"}          │ │
│   │ (5) append user{Parts: [tool_result]}                     │ │
│   │     trail = [user, asst#0, user-result]                   │ │
│   └────────────────────────────────────────────────────────────┘ │
│                                                                 │
│   ┌─ iter 1 ──────────────────────────────────────────────────┐ │
│   │ (1) req := messagesToProvider(trail) ← now 3 messages    │ │
│   │ (2) Provider.Stream → "echo returned hi. done."           │ │
│   │     → assistant = Message{Parts: [text]}                  │ │
│   │ (3) collectToolUses(...) → []                             │ │
│   │     ★ no tool_use → return trail, nil                    │ │
│   └────────────────────────────────────────────────────────────┘ │
│                                                                 │
│   final trail = [user, asst#0, user-result, asst#1]             │
│   Provider.Stream call count = 2                                │
└─────────────────────────────────────────────────────────────────┘
```

**Five load-bearing decisions**:

1. **One Provider.Stream call per iteration; drain serially before running tools**. Upstream's `processor.ts` dispatches tools as they arrive in the stream ("tool-call" event triggers a fork-and-run, concurrently). We simplify to "drain the entire assistant message, THEN run each tool serially." Cost of the simplification: tools can't start running while the assistant is still streaming (a bit less latency overlap). Benefit: `runOneTool` is a synchronous Go function — no Deferred / Promise / channel plumbing.
2. **Tools run sequentially**. An assistant message with 3 tool_uses runs them one at a time, not in parallel. Upstream is also sequential (each tool's snapshot/permission handshake is naturally serial). A future ext-exercise could add errgroup — the Orchestrator's contract wouldn't change, only `runOneTool`'s call loop would.
3. **Permission deny is NOT a Run-level error**. Run returns nil err, trail is complete, and the tool_result Part has IsError=true. The LLM sees it was denied, and typically ends the turn next iteration. This is the *agent self-correction* mechanism — bubbling deny up as a transport error would break the correction loop.
4. **Tool errors work the same way**. `Tool.Execute` returning err → `tool_result{IsError:true, err.Error()}`. The LLM reads its own failure and corrects. This is what lets `DieTool` (always fails) NOT crash Run — it just produces an IsError feedback the LLM observes.
5. **MaxIterations is a simple cap**. Production opencode bounds via token / cost budget (s14's job), but a bound is always present — unbounded LLM agent loops are how you wake up to a $400 bill. We use the simplest possible iteration counter: when iter==MaxIterations, return ErrMaxIterationsExceeded; the trail captures "what we had at cap-hit time," and the caller can re-Run with a higher cap to pick up where they left off.

**Why ~600 LOC**: because this is the *first* time 4 mechanisms (Parts/Tool/Permission/Streaming) come together, and each must be re-implemented in this module (per curriculum rule, every chapter is self-contained, zero cross-chapter imports). `parts.go` copies s02, `provider.go` copies s06, `permission.go` copies s04, `tool.go` copies s03 — that's ~400 lines. `loop.go` adds `Orchestrator` + `runOneTool` + `consumeOne` + `messagesToProvider` for ~250 lines. fakes + tools + tests ~200 lines. Roughly the right total.

## What Changed (vs. s06)

s06's `Loop.Consume` does one thing: drain one stream, exit. s10's `Orchestrator.Run` actually *invokes* tools and *comes back*:

```diff
 // s06: one stream, stop. tool_use is just a Part — no one executes it.
-loop := &Loop{Provider: provider}
-msg, err := loop.Consume(ctx, req)
-// msg.Parts may contain PartToolUse — caller is on their own
-// (s06's tests stop here and assert that Parts has the right shape)

+// s10: multi-turn loop, runs tools and feeds results back automatically.
+orch := &Orchestrator{
+    Provider:      provider,
+    Tools:         tools,
+    Permissions:   merged, // s09's cascade result
+    MaxIterations: 10,
+}
+trail, err := orch.Run(ctx, []Message{userMsg})
+// trail has the full multi-turn history: user → asst → user-result → asst → ...
+// Every PartToolUse was *executed*, permission *checked*, result *fed back* to the LLM.
```

`Loop.Consume` → `Orchestrator.consumeOne` is nearly a 1:1 copy (same streaming assembly algorithm — adjacent text concatenated, tool_use breaks the run, reasoning concatenated the same way). The difference: `consumeOne` is *Run's helper*, no longer the top-level entry — there's a for loop above it.

**Permission interface evolution**: s04 designed `Evaluate(perm, target, ...rulesets)` to take variadic rulesets (cascade flattening at the call site). s09's `MergePermissions` produces a *flat* slice. s10 connects the two layers: `Orchestrator.Permissions` IS the *already-cascaded* flat ruleset; each tool dispatch calls `Evaluate(name, target, o.Permissions)` directly, the evaluator is unaware of cascading. This delivers on s09's "cascade is structural, evaluator is semantic" promise.

**Tool interface didn't change at all**. s03 designed `Tool.Execute(ctx, input) (string, error)` with ctx and err already in place; s10 is the first consumer that actually uses ctx (cancel propagation) + err (in-band tool_result IsError). Had s03 made Execute return only string with no err, s10 would've been forced to add panic-recovery or worse — a vindication of "right interface design one chapter early pays off the next chapter."

**Where s11-s14 go from here**:
- s11 (skills) injects SKILL.md content into the system prompt — doesn't change Orchestrator, changes how Request.System is computed.
- s12 (mcp) lets Registry hold remote tools — doesn't change Orchestrator, changes Tool implementations.
- s13 (lsp) same as s12.
- s14 (cost & recovery) wraps Run in a retry wrapper, accumulates Usage for billing — doesn't change Orchestrator internals, adds an outer wrapper.

s10's Orchestrator is the *carrier* for every later refinement.

## Try It

```bash
cd agents/s10-tool-loop

# Demo (deterministic, no network, 2 stream rounds):
go run .

# 5 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 5 tests cover:

1. **ZeroToolConversationCompletes** — assistant emits text only; no tool_use; loop exits after 1 iteration. `provider.callCount==1`, trail length 2 (initial + assistant). The baseline.
2. **OneToolRoundTrip** — assistant tool_use("echo") → tool_result → assistant end_turn. Trail length 4, Stream called twice. Verifies the end-to-end Provider.Stream → Tool.Execute → next Provider.Stream sees result chain.
3. **TwoConsecutiveToolCalls** — iter 1: echo("first") → iter 2: echo("second") → iter 3: end_turn. Trail length 6 (initial + 3 asst + 2 user-result), Stream called 3 times. Proves inter-iteration trail growth is correct.
4. **PermissionDenyProducesErrorResult** — ruleset `echo:* deny`. Tool **not executed**, but Run returns nil err, trail's tool_result Part has `IsError=true` + "permission denied". Proves deny is in-band feedback.
5. **MaxIterationsExceeded** — assistant always asks for echo, `MaxIterations: 1`. Run returns `ErrMaxIterationsExceeded`, Stream called only once (cap fires before iter 1 starts), trail's last entry is the synthesized user-results Message — caller can re-Run with a higher cap to resume.

## Upstream Source Reading

s10 mirrors `packages/opencode/src/session/processor.ts`. The full file is 837 lines, one of the *most* complex single files in opencode — it ties together LLM streaming, tool dispatch, permission asking, snapshots, retry policy, error recovery, the event system. s10 takes the *core skeleton* (Result + Handle + ProcessorContext + process), leaving snapshot / retry / event system / overflow for later chapters.

```ts
// upstream:packages/opencode/src/session/processor.ts L34-L82 + L734-L802

// L34 — three terminating verdicts. We simplify to (trail, err); compact /
// continue don't appear in our interface (continue is the caller's
// implicit behavior in the for loop, compact is s14's job).
export type Result = "compact" | "stop" | "continue"

// L38-L54 — Handle interface. process(streamInput) runs ONE iteration and
// returns a Result; the caller (session.ts) decides whether to call process
// again. Our Go Orchestrator.Run runs *all* iterations internally and
// doesn't expose a Handle.
export interface Handle {
  readonly message: MessageV2.Assistant
  readonly updateToolCall: (
    toolCallID: string,
    update: (part: MessageV2.ToolPart) => MessageV2.ToolPart,
  ) => Effect.Effect<MessageV2.ToolPart | undefined>
  readonly completeToolCall: (
    toolCallID: string,
    output: { title: string; metadata: Record<string, any>; output: string; attachments?: MessageV2.FilePart[] },
  ) => Effect.Effect<void>
  readonly process: (streamInput: LLM.StreamInput) => Effect.Effect<Result>
}

// L73-L82 — per-Run mutable state. Important: toolcalls is a dict — because
// upstream's tools run *concurrently* ("tool-call" event arrives → fork
// a Promise to run it), and need callID-indexed lookup when results come
// back. Our Go side runs serially → no dict needed.
interface ProcessorContext extends Input {
  toolcalls: Record<string, ToolCall>
  shouldBreak: boolean
  snapshot: string | undefined
  blocked: boolean
  needsCompaction: boolean
  currentText: MessageV2.TextPart | undefined
  reasoningMap: Record<string, MessageV2.ReasoningPart>
}

// L734-L802 — the actual process function. Three regions:
//   (a) build stream + Stream.tap(handleEvent) + Stream.takeUntil(needsCompaction)
//   (b) wrap with Effect.retry(SessionRetry.policy) (s14's territory)
//   (c) end-of-function Result branching
const process = Effect.fn("SessionProcessor.process")(function* (streamInput: LLM.StreamInput) {
  ctx.needsCompaction = false
  ctx.shouldBreak = (yield* config.get()).experimental?.continue_loop_on_deny !== true

  return yield* Effect.gen(function* () {
    yield* Effect.gen(function* () {
      ctx.currentText = undefined
      ctx.reasoningMap = {}
      const stream = llm.stream(streamInput)

      yield* stream.pipe(
        Stream.tap((event) => handleEvent(event)),     // ← per-event dispatch
        Stream.takeUntil(() => ctx.needsCompaction),    // ← early exit on overflow
        Stream.runDrain,
      )
    }).pipe(
      Effect.onInterrupt(() => Effect.gen(function* () {
        aborted = true
        if (!ctx.assistantMessage.error) yield* halt(new DOMException("Aborted", "AbortError"))
      })),
      Effect.retry(SessionRetry.policy({ /* ... s14 ... */ })),
      Effect.catch(halt),
      Effect.ensuring(cleanup()),
    )

    // ★ Result tri-branch — this is what s10 Orchestrator.Run's tail mirrors
    if (ctx.needsCompaction) return "compact"               // s14 will add
    if (ctx.blocked || ctx.assistantMessage.error) return "stop"  // our nil-err return
    return "continue"                                        // our for-loop continues
  })
})

// L336-L395 — "tool-call" event handler. This is the *concurrent* tool
// dispatch site (the actual execute happens inside the AI SDK, triggered
// asynchronously by the stream pipeline; processor here just updates Part
// state and runs the doom-loop check).
//
// L370-L394 is the doom-loop detector: look at the last 3 parts; if all
// the same tool with the same input → ask the user via permission.ask. We
// don't port this; s14's retry classification will use a similar mechanism.
case "tool-call": {
  if (ctx.assistantMessage.summary) {
    throw new Error(`Tool call not allowed while generating summary: ${value.toolName}`)
  }
  yield* updateToolCall(value.toolCallId, (match) => ({
    ...match,
    tool: value.toolName,
    state: { ...match.state, status: "running", input: value.input, time: { start: Date.now() } },
  }))

  // doom-loop: same tool, same input, 3 in a row → ask the user
  const parts = MessageV2.parts(ctx.assistantMessage.id)
  const recentParts = parts.slice(-DOOM_LOOP_THRESHOLD)
  if (
    recentParts.length === DOOM_LOOP_THRESHOLD &&
    recentParts.every(
      (part) => part.type === "tool" && part.tool === value.toolName &&
                part.state.status !== "pending" &&
                JSON.stringify(part.state.input) === JSON.stringify(value.input),
    )
  ) {
    const agent = yield* agents.get(ctx.assistantMessage.agent)
    yield* permission.ask({
      permission: "doom_loop",
      patterns: [value.toolName],
      sessionID: ctx.assistantMessage.sessionID,
      metadata: { tool: value.toolName, input: value.input },
      always: [value.toolName],
      ruleset: agent.permission,
    })
  }
  return
}
```

Line-by-line annotation (key lines):

- **L34 Result enum** — three terminating verdicts. We drop "continue" (becomes implicit continuation of the for loop) and "compact" (s14's job), keeping just "stop" (nil-err return) and "cap hit" (ErrMaxIterationsExceeded).
- **L38-L54 Handle interface** — upstream designs process as a *single-step* operation, with the caller calling repeatedly until Result != "continue". This shape lets the caller insert logic between steps (cancel, UI updates, persistence). Our Go side bundles everything: Orchestrator.Run runs the whole loop internally and returns the complete trail. Trade-off: less mid-loop control for the caller (cancellation must go through ctx), more compact API surface in exchange.
- **L73-L82 ProcessorContext** — *shared mutable state across one process call*. Note `toolcalls` is a dict — because upstream's tools run *concurrently*, "tool-call" event triggers a forked Promise, and results need callID-indexed lookup when they return. Serial execution removes the need — our `runOneTool` is a synchronous function.
- **L734-L745 stream.pipe** — `Stream.tap(handleEvent)` is the key: every event passes through handleEvent, which is one big switch handling "text-delta" / "tool-call" / "tool-result" etc. Our Go side keeps "switch on event type" inside `consumeOne` (drain phase), and lifts "tool dispatch" out into `runOneTool` (post-drain) — because we're not running tools concurrently, we can split these two phases.
- **L750-L760 Effect.onInterrupt + Effect.retry + Effect.catch** — three layers of error handling: abort, retry policy (s14), final halt. Our Go side does only the simplest ctx.Err() check — retry / overflow are s14's job.
- **L798-L800 Result tri-branch** — the part s10 Orchestrator.Run mirrors most directly:
  - `if (ctx.needsCompaction) return "compact"` → we don't have this
  - `if (ctx.blocked || ctx.assistantMessage.error) return "stop"` → our "natural end_turn → return trail, nil"
  - `return "continue"` → our "for iter continues"
- **L370-L394 doom-loop detector** — upstream's "same tool, same input, 3 in a row" guard. We don't ship this (s14's retry classification will use a similar mechanism). Note L386-L394's `permission.ask` is *blocking* (waits on UI for user reply) — headless s10 has no UI, so we drop the whole branch.

Permalinks:

- Result + Handle (L34-L54): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L34-L54>
- ProcessorContext (L73-L82): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L73-L82>
- tool-call handler + doom-loop (L336-L395): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L336-L395>
- process function (L734-L802): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L734-L802>

What we kept, what we cut:

- **Kept** — the 5-step iteration skeleton (stream → drain → tool dispatch → result feedback → loop), Permission gate before tool execute, tool error as in-band feedback, MaxIterations safety cap, Stream.Close via defer.
- **Cut for now** — the entire Effect runtime / Layer / Service stack (Go uses plain struct + ctx), Snapshot persistence (s07 territory), Plugin.trigger hooks (s11+ territory), SessionRetry policy (s14), isOverflow / needsCompaction → compaction trigger (s14), DOOM_LOOP_THRESHOLD detection (s14 will do similar), SessionEvent v2 dual-write (entire v2 schema is separate), concurrent tool dispatch (we run serially).
- **Forward-compat** — adding a retry wrapper doesn't require changes inside Orchestrator (wrap from outside); adding cost tracking just needs to consume the Usage field on each assistant Message in trail; adding concurrent tools just needs `runOneTool`'s call loop to become an errgroup. The 5-step skeleton stays.

opencode session-processor reading order:

1. `packages/opencode/src/session/processor.ts` L34-L82 — Result / Handle / ProcessorContext shape (parent of s10's Orchestrator; this section's body)
2. `packages/opencode/src/session/processor.ts` L734-L802 — the process function (most direct mirror of s10's Orchestrator.Run)
3. `packages/opencode/src/session/processor.ts` L229-L640 — handleEvent's big switch (s10 split it: "text/reasoning/tool-input" went into consumeOne, "tool-call/result/error" went into runOneTool)
4. `packages/opencode/src/session/llm.ts` L100-L200 — LLM.stream implementation (parent of s06's mirror; s10 reuses s06's streaming assembly algorithm)
5. `packages/opencode/src/permission/index.ts` — `permission.ask` implementation (s10 simplifies "Ask" → "Allow"; the real path waits on user reply)
