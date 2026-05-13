# s05 — provider-iface

s01 gave us a single `CreateMessage(ctx, req) (*Resp, error)` blocking call —
enough to confirm Anthropic's wire format and zero things more. s05 swaps
that one return value for two: a `Provider` interface (the abstraction every
later session and every Phase G vendor implements) and a `Stream` (the
pull-based iterator that lets the loop make decisions mid-response).

The mechanism: `Stream(ctx, Request) (Stream, error)` returns a thing whose
`Next()` yields one `Event` at a time — text deltas, fully-buffered tool
calls, reasoning chunks, and one terminal `EventFinish` carrying the usage
tally — until `io.EOF`. The Anthropic implementation parses Server-Sent
Events from `/v1/messages?stream=true` and translates each upstream event
type (`message_start` / `content_block_start` / `content_block_delta` /
`content_block_stop` / `message_delta` / `message_stop`) into our union.

## Files

- `provider.go` — interface + cross-vendor wire types:
  - `Provider` interface (one method, `Stream`)
  - `Request` (Model, System, Messages, Tools, MaxTokens, Temperature)
  - `Stream` interface (`Next() (Event, error)` returning `io.EOF` when done; `Close() error`)
  - `EventType` enum (`EventText` / `EventToolUse` / `EventReasoning` / `EventFinish`)
  - `Event` struct (only the field matching `Type` is populated)
  - `Message`, `ContentBlock`, `ToolSchema`, `ToolUseRef`, `Usage` — JSON wire types
- `provider_anthropic.go` — the concrete impl:
  - `AnthropicProvider` + `NewAnthropicProvider(apiKey, model)`
  - test helper `withBaseURL(url)` for httptest
  - `Stream(...)` POSTs to `<baseURL>/v1/messages` with `"stream": true`, `Accept: text/event-stream`
  - `*anthropicStream` reads SSE events, buffers tool_use input deltas across
    `content_block_delta` events, joins `message_start` input tokens with
    `message_delta` output tokens for the final `EventFinish.Usage`
- `main.go` — short demo: builds a Request, calls Stream, prints each Event
  as it arrives. Bound to the `Provider` interface (not the concrete struct)
  to demonstrate that Phase G's `OpenAIProvider` drops in unchanged.
- `provider_test.go` — 4 tests using `httptest.NewServer` to return canned
  SSE bytes:
  1. **text-only stream** → sequence of `EventText` events
  2. **tool_use stream** → ONE `EventToolUse` with name + buffered input parsed
  3. **reasoning stream** → `EventReasoning` event from the `thinking` block
  4. **message_stop → EventFinish + io.EOF** — pins the two-call contract
     consumers rely on (`for { … if errors.Is(err, io.EOF) { break } }`)

## Run

```
export ANTHROPIC_API_KEY=sk-ant-...
go run . hello in three words

# 4 tests, no network — httptest stubs return canned SSE bytes.
go test -count=1 ./...
```

## What this maps to upstream

| This file | Upstream file |
|---|---|
| `provider.go` `Provider` / `Stream` | `packages/opencode/src/provider/provider.ts` (the `model()` factory + AI SDK's `streamText`) |
| `provider_anthropic.go` SSE reader | `packages/opencode/src/provider/provider.ts` `wrapSSE()` (lines 39–85) + AI SDK's Anthropic adapter |
| `provider_anthropic.go` event translation | what `@ai-sdk/anthropic` does internally — opencode delegates; we do it by hand |
| `provider.go` `Request` / `Message` | `packages/opencode/src/session/llm.ts` `streamText` argument shape |

## Key teaching points

- **The interface is `Stream`, not `Send + Get`.** Every consumer (s06's
  reducer, s10's tool loop, s14's retry wrapper) needs to react mid-response
  — a permission verdict for a tool call should NOT wait for the rest of the
  message to finish. So the abstraction is "iterator of events," not "future
  of full response."
- **`Next()` returns `io.EOF` for clean end.** This is the Go-idiomatic
  end-of-stream signal — same shape as `(*os.File).Read`, `(*bufio.Scanner).Scan`,
  channel-close idioms. s06's loop will be `for { ev, err := stream.Next();
  if errors.Is(err, io.EOF) { break } }` and that pattern is contractual.
- **Tool input is buffered across deltas.** Anthropic streams tool_use input
  as N `input_json_delta` chunks. We hold them in `contentBlockBuffer.jsonAcc`
  and emit ONE `EventToolUse` at `content_block_stop`. The alternative —
  surfacing per-token deltas — would force every consumer to reimplement the
  same buffering, and you can't dispatch a tool call on half its arguments.
- **Usage is split across two SSE events.** `message_start` carries
  `input_tokens`; `message_delta` carries `output_tokens`. We re-join them
  before the `EventFinish`. Consumers see one `Usage` per stream.
- **`EventFinish` then `io.EOF` is two separate `Next()` calls.** The finish
  event carries data the caller needs (usage); EOF is the loop-exit signal.
  Conflating them would force a "did I get usage yet?" flag in every consumer.

## What changed vs s01

s01 had one method; s05 has one method that returns a stream. The wire
shape is otherwise the same — POST `/v1/messages`, same headers, same JSON
body — plus `"stream": true` and `Accept: text/event-stream`:

```diff
 // s01: blocking single round trip.
-resp, err := p.CreateMessage(ctx, req)
-for _, b := range resp.Content {
-    if b.Type == "text" { fmt.Println(b.Text) }
-}

+// s05: pull-based stream of events, decided one at a time.
+stream, err := p.Stream(ctx, req)
+defer stream.Close()
+for {
+    ev, err := stream.Next()
+    if errors.Is(err, io.EOF) { break }
+    if err != nil { return err }
+    switch ev.Type {
+    case EventText:    fmt.Print(ev.Text)
+    case EventToolUse: handleTool(ev.ToolUse)        // s10 dispatches here
+    case EventFinish:  recordUsage(ev.Usage)         // s14 bills here
+    }
+}
```

s06 will replace the `switch` with a Part-aggregating reducer (`Event` →
`Part` → `Message`); s10 will replace `handleTool` with a real
permission-check + Registry-dispatch + result-feedback loop. The
`p.Stream(ctx, req)` call site stays unchanged — that's the whole point.

See `docs/zh/s05-provider-iface.md` and `docs/en/s05-provider-iface.md` for
the long-form walkthrough, including the upstream `BUNDLED_PROVIDERS` map
that motivates why the Go translation has one interface + factory funcs
instead of one runtime registry of strings.
