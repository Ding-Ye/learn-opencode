// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/processor.ts (Handle / process orchestration, L34-L150 + L734-L802)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L34-L150
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s10 cares about this file:
//   processor.ts is THE orchestration — the integration that takes a
//   prepared StreamInput, runs the LLM stream, dispatches tool calls
//   as they arrive, settles each tool's deferred, persists Parts, and
//   returns one of "compact" | "stop" | "continue". Our s10 Orchestrator
//   is a Go-port simplification: same per-iteration shape (stream → tool
//   dispatch → maybe-continue), no Effect runtime / Layer / Service, no
//   persistence (s07 owns that), no retry / overflow detection (s14).
//
// What we ported (s10):
//   - `Handle.process(streamInput) → "compact"|"stop"|"continue"`
//     ↓
//     `Orchestrator.Run(ctx, initial) → ([]Message, error)` — the
//     simplification: rather than Result enum, we return the trail and
//     a sentinel error (ErrMaxIterationsExceeded) for the cap case.
//   - the per-event handler's "tool-call" + "tool-result" + "tool-error"
//     branches → `runOneTool` in loop.go (one synchronous step that
//     writes one ToolResultPart).
//   - the streaming assembly (text-start / text-delta / text-end +
//     reasoning-* + tool-input-*) → `consumeOne` in loop.go (same
//     algorithm s06 already taught).
//   - the implicit "if no tool calls, stop" termination → our explicit
//     `if len(toolUses) == 0 { return trail, nil }` after each iteration.
//
// What we DID NOT port (out of scope):
//   - Effect.gen / Layer / Service / Deferred — Go uses plain structs +
//     channels (which we don't even need at s10's scope, since there's
//     no concurrency).
//   - Snapshot.track / patch — s07 territory (persistence + revert).
//   - Plugin.trigger("experimental.text.complete", ...) — s11+ territory.
//   - SessionRetry.policy / Effect.retry — s14's job.
//   - isOverflow + ctx.needsCompaction — s14's compaction signal.
//   - DOOM_LOOP_THRESHOLD = 3 — the "model called same tool 3 times with
//     same args; ask user" detector. Out of scope; s14's retry classification.
//   - SessionEvent dual-write to v2 event stream — entire v2 schema is
//     out of scope.
//
// The 80 lines below are the heart of the orchestration: the Result enum,
// the Handle interface, the ProcessorContext shape, and the load-bearing
// `process` function (L734-L802) that runs the stream and returns the
// "should we loop again?" verdict.

// L34 — the result enum. Three terminating verdicts:
//   "compact"  — context window overflowed; caller should compact and re-Run.
//   "stop"     — natural end_turn OR a permission/question rejection that
//                set ctx.blocked. Caller stops.
//   "continue" — assistant emitted tool calls; caller should re-Run.
// Our Go simplification: Run returns the full trail, plus an error.
// Termination shape: nil err = natural stop; ErrMaxIterationsExceeded
// = the rough equivalent of upstream's "shouldn't continue forever" guard.
// We don't surface "compact" because s10 doesn't track usage.
export type Result = "compact" | "stop" | "continue"

export type Event = LLM.Event

// L38-L54 — the Handle interface. Three operations:
//   - message: the assistant Message-in-progress (read-only accessor).
//   - updateToolCall(callID, fn): mutate one tool's Part state mid-flight
//                                  (e.g. "input deltas arrived"; "running";
//                                  "completed").
//   - completeToolCall(callID, output): mark tool done with output.
//   - process(streamInput): run one iteration, return the verdict.
// Our Orchestrator collapses all of these into Run(): the assistant
// message is built and returned in the trail; tool state mutation isn't
// observable to callers (we don't expose a Handle); process() runs the
// FULL multi-iteration loop, not just one iteration.
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

// L73-L82 — the per-Run mutable state. Tracks all in-flight tool calls
// (so out-of-order tool-result events can find their pending Part), the
// current text part being streamed, the reasoning map (keyed by reasoning
// block id), and three flags: shouldBreak (denial behavior), blocked
// (a permission rejection happened), needsCompaction (overflow detected).
//
// Our Go equivalent is much smaller: the Orchestrator's state is just
// the running []Message trail. We don't track tool calls in a dict
// because we run them SEQUENTIALLY after the stream drains, not
// concurrently while the stream is still arriving. That's the load-
// bearing simplification — opencode dispatches each tool the moment its
// "tool-call" event arrives (which means tools can run in parallel with
// later text deltas); we wait for the whole assistant message to finish
// streaming before running ANY tool.
interface ProcessorContext extends Input {
  toolcalls: Record<string, ToolCall>
  shouldBreak: boolean
  snapshot: string | undefined
  blocked: boolean
  needsCompaction: boolean
  currentText: MessageV2.TextPart | undefined
  reasoningMap: Record<string, MessageV2.ReasoningPart>
}

// L734-L802 — the LOAD-BEARING `process` function. Stream the LLM,
// pipe each event through `handleEvent`, stop early if needsCompaction
// flips, retry transport errors via SessionRetry.policy, return the
// verdict. The verdict logic at the end (L798-L800) is the part s10's
// Run mirrors most closely:
//
//   if (ctx.needsCompaction) return "compact"
//   if (ctx.blocked || ctx.assistantMessage.error) return "stop"
//   return "continue"
//
// Our Go translation:
//   - "compact" branch: omitted (s14 will add it).
//   - "stop" branch: our `if len(toolUses) == 0 { return trail, nil }`
//     after each iteration is the equivalent — natural end_turn OR a
//     denial that the model decided not to retry.
//   - "continue" branch: our `for iter := ...` keeps looping (which is
//     the *caller's* behavior in upstream — outside processor.ts, in
//     session.ts, the caller checks the Result and re-invokes process).
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
        Stream.runDrain,                                // ← drain the rest
      )
    }).pipe(
      Effect.onInterrupt(() => Effect.gen(function* () {
        aborted = true
        if (!ctx.assistantMessage.error) yield* halt(new DOMException("Aborted", "AbortError"))
      })),
      Effect.catchCauseIf(
        (cause) => !Cause.hasInterruptsOnly(cause),
        (cause) => Effect.fail(Cause.squash(cause)),
      ),
      Effect.retry(SessionRetry.policy({ /* ... */ })),  // ← s14's job
      Effect.catch(halt),
      Effect.ensuring(cleanup()),
    )

    if (ctx.needsCompaction) return "compact"
    if (ctx.blocked || ctx.assistantMessage.error) return "stop"
    return "continue"
  })
})

// L336-L395 — the "tool-call" event handler. This is upstream's
// per-tool-use dispatch site. Note the DOOM_LOOP detection at L370-L394:
// look at the LAST 3 parts; if they're all the same tool with the same
// input, ask the user. We don't port this; s14 will add a similar guard
// using its retry classification machinery.
case "tool-call": {
  if (ctx.assistantMessage.summary) {
    throw new Error(`Tool call not allowed while generating summary: ${value.toolName}`)
  }
  // ... write to Part state ...
  yield* updateToolCall(value.toolCallId, (match) => ({
    ...match,
    tool: value.toolName,
    state: { ...match.state, status: "running", input: value.input, time: { start: Date.now() } },
  }))

  // DOOM_LOOP detection — same tool, same input, 3 in a row → ask user.
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

// END EXCERPT — the file continues with tool-result, tool-error, error,
// start-step, finish-step, text-*, finish, default branches (L396-L640),
// then cleanup() (L645-L703), halt() (L705-L732), the layer construction
// (L818-L834). All of those are out of s10's scope; the Result enum,
// Handle interface, ProcessorContext shape, process function, and
// tool-call dispatch above are the part we Go-port.
