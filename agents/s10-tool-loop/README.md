# s10 — tool-loop

s06 assembled a stream of Events into one assistant Message. s09 built
the agent + permission cascade. s10 is **the integration**: an
`Orchestrator` that takes a Provider + Tool registry + permission
ruleset + initial messages and runs the full tool-execution loop until
the assistant emits no more tool calls (or hits `MaxIterations`).

This is the first session that ties FOUR mechanisms together — Parts
(s02), Tool dispatch (s03), Permission evaluation (s04), Streaming
(s06) — and shows how they compose into the actual agent loop. After
s10, every later session (s11 skills / s12 mcp / s13 lsp / s14 cost) is
a refinement applied to this loop, not a new mechanism.

## The 5-step iteration

```
Run(ctx, initial) →
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

Mirrors `Handle.process` in opencode's
`packages/opencode/src/session/processor.ts` L734-L802 — same shape
(per-iteration: stream → tool dispatch → maybe-continue), the same
terminating conditions (no tool calls → "stop"; needsCompaction →
halt), expressed as a plain Go for-loop instead of an Effect runtime
pipeline.

## Files

- `parts.go` — Message + Part union (re-implemented from s02 / s06).
  s10 is the first session to construct PartToolResult Parts, which
  go in the synthesized user Message between iterations.
- `provider.go` — Provider / Stream / Event / Request / Usage
  (re-implemented from s05 / s06 verbatim).
- `permission.go` — Action / Rule / Ruleset / Evaluate
  (re-implemented from s04 verbatim, last-match-wins).
- `tool.go` — Tool interface + Registry (re-implemented from s03).
- `tool_echo.go` — `EchoTool`: returns its `text` input verbatim. Used
  by every "happy path" test.
- `tool_die.go` — `DieTool`: always returns an error. Used to test the
  tool-error → tool_result conversion (kept available; not used in the
  current 5 tests but provides a fixture for future ext-exercises).
- `fake_provider.go` — `fakeProvider` with a SLICE OF EVENT SLICES
  (one slice per Stream call, indexed by call count). The s10
  generalization of s06's single-stream fake.
- `loop.go` — the `Orchestrator` struct + `Run` method + helpers:
  - `runOneTool` — the per-tool-use inner step. Permission gate +
    tool dispatch, all flattened to a `*ToolResultPart`.
  - `consumeOne` — drains exactly ONE Provider.Stream call into ONE
    assistant Message (same assembly rules as s06).
  - `collectToolUses` — walks an assistant Message's Parts, returns
    pointers to every tool_use in order.
  - `messagesToProvider` — translates `[]Message` (in-memory,
    Parts-based) into `[]ProviderMessage` (wire-shape, ContentBlocks).
  - `toolSchemas` — projects the Registry into Request.Tools.
- `main.go` — short demo: 1 echo tool, scripted 2-iteration Provider,
  prints the full Message trail as JSON.
- `loop_test.go` — 5 tests:
  1. **ZeroToolConversationCompletes** — assistant emits text only;
     loop exits after 1 Stream call. The baseline.
  2. **OneToolRoundTrip** — assistant tool_use → tool_result → assistant
     end_turn. Trail length 4, Stream called twice.
  3. **TwoConsecutiveToolCalls** — three iterations (tool, tool,
     end_turn). Pins the inter-iteration trail growth.
  4. **PermissionDenyProducesErrorResult** — `echo:* deny` ruleset.
     Tool is NOT executed; tool_result Part has IsError=true and
     "permission denied". Run returns nil error (deny is in-band).
  5. **MaxIterationsExceeded** — assistant always asks for echo;
     `MaxIterations: 1`. Run returns ErrMaxIterationsExceeded after
     iter 0 + 1 user-results message; Stream called once.

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

- **The loop is bounded.** `MaxIterations` is the simplest possible
  termination guard. Production opencode bounds via a token / cost
  budget instead (s14's job), but the bound is always present —
  unbounded LLM agent loops are how you wake up to a $400 bill.
- **Permission deny is in-band.** A denied tool produces a
  `tool_result{IsError:true, "permission denied"}` Part that goes back
  to the LLM in the next iteration. The model SEES the denial and
  recovers (typically by ending the turn or trying a different
  approach). Deny is NOT a Run-level error.
- **Tool errors are also in-band.** Same pattern: `Tool.Execute`
  returning an error becomes `tool_result{IsError:true, err.Error()}`,
  not a Run failure. The LLM gets to read its own mistake and try
  again.
- **One Stream call per iteration.** The Orchestrator never calls
  Provider.Stream concurrently — opencode runs tools sequentially
  too. Parallel tool execution is a later optimization (the
  Orchestrator's contract wouldn't change; only `runOneTool`'s call
  loop would).
- **The trail is the result.** `Run` returns `[]Message` — the
  caller is responsible for persistence (s07), display, or feeding
  the trail back into a fresh Run with a higher cap. The
  Orchestrator is stateless between Run calls.
- **Reasoning Parts are dropped on the way back to the LLM.**
  `messagesToProvider` skips `PartReasoning` — the LLM doesn't see
  its own past thinking when re-encoding the request. That matches
  upstream's behavior (the v2 events stream surfaces reasoning to
  the user / persistence layer, not to the next request prompt).

See `docs/zh/s10-tool-loop.md` and `docs/en/s10-tool-loop.md` for the
long-form walkthrough plus the upstream `processor.ts` excerpt that
motivates the per-iteration shape.
