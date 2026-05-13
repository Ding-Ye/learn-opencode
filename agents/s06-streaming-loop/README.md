# s06 — streaming-loop

s05 gave us a `Provider.Stream(ctx, req) (Stream, error)` that yields one
`Event` at a time — text deltas, fully-buffered tool calls, reasoning
chunks, a final `EventFinish`. s06 puts a *consumer* on the other end: the
`Loop`, which assembles those Events into one `Message` of `Parts` —
collapsing N adjacent text deltas into one `TextPart`, recording each
tool_use as one `ToolUsePart`, joining N reasoning chunks into one
`ReasoningPart`, and recording the final `Usage` on the Message.

The Loop is deliberately narrow: it does not dispatch tools (s10), does not
check permissions (s04 / s10), does not persist (s07), does not retry
(s14). Keeping it that small is what lets each later session add one
concern without rewriting the streaming layer.

## Files

- `provider.go` — Provider / Stream / Event re-implemented from s05
  (each session is its own Go module, no cross-session imports).
- `parts.go` — `Message` + `Part` tagged union re-implemented from s02.
  Variant payloads are `TextPart` / `ToolUsePart` / `ToolResultPart` /
  `ReasoningPart` (suffix-Part to avoid colliding with the same-named
  PartKind constants).
- `loop.go` — the new code:
  - `type Loop struct { Provider Provider }`
  - `func (l *Loop) Consume(ctx context.Context, req Request) (*Message, error)`
  - opens `Provider.Stream`, then in a `for`-loop calls `Next()` until
    `io.EOF`, accumulating each Event into a `*Message`.
  - on `ctx.Err() != nil` (cancel / deadline), returns `ctx.Err()`
    without leaking a partial Message.
  - on a malformed Event (e.g. `EventToolUse` missing `Name`), returns
    a clear error.
- `fake_provider.go` — test helper. `fakeProvider{events, errAt, blockOn,
  …}` replays a scripted slice of Events through the Stream interface;
  the `blockOn` channel is the abort test's hook for "guarantee we're
  mid-stream when Cancel fires."
- `main.go` — short demo. Builds a fakeProvider with a small scripted
  stream, calls `Loop.Consume`, prints the assembled Message as JSON.
  Deterministic, no network.
- `loop_test.go` — 4 tests, all using `fakeProvider`:
  1. **text-only stream** → 1 `TextPart` (N deltas collapsed).
  2. **interleaved tool_use + text** → 3 Parts (text, tool_use, text) in
     order; the tool_use breaks the text run.
  3. **AbortContext mid-stream** → returns `context.Canceled`, no
     partial Message.
  4. **malformed Event** (`EventToolUse` without name) → returns clear
     error mentioning "tool name."

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

- **The Loop is the bridge between two layers.** s05's Stream is "what
  the wire delivered next"; the Message is "what the rest of the agent
  operates on." Without an explicit assembler, every consumer would
  re-implement the same delta-accumulation logic.
- **Adjacent same-kind events collapse; different-kind events break the
  run.** N text deltas become one `TextPart`; a tool_use between them
  starts a fresh `TextPart` for the text after. This is the load-bearing
  rule for s10's "did the model finish or did it ask for a tool?" check.
- **`EventFinish` records Usage; the next `Next()` returns `io.EOF`.**
  Same two-call contract s05 established — Loop relies on it for clean
  termination.
- **Cancellation returns `ctx.Err()`, not a partial Message.** s10's
  Ctrl-C handling depends on this. Returning `(half-msg, err)` would
  tempt callers to use the half-msg anyway.
- **Malformed Events fail fast.** A tool_use without a Name would push
  the bug to s10's dispatcher, where `unknown tool ''` is much harder to
  trace back. The Loop is the right boundary to validate.

See `docs/zh/s06-streaming-loop.md` and `docs/en/s06-streaming-loop.md`
for the long-form walkthrough plus the upstream `session/llm.ts` excerpt
that motivates the design.
