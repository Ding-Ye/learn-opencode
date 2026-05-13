---
title: "s05 · Provider abstraction"
chapter: 5
slug: s05-provider-iface
est_read_min: 11
---

# s05 · Provider abstraction

> What this teaches: upgrade s01's *one blocking call* into *interface + stream*. This is the LLM-call shape every later session relies on — a `Provider` interface, a `Stream` iterator, plus Anthropic's SSE parser as the first concrete implementation. When Phase G adds OpenAI, callers don't change; only a new struct lands.

---

## Problem

s01's `CreateMessage(ctx, req) (*Resp, error)` is enough for two things: confirm Anthropic's wire format, and run the happy path once. Past that it falls over:

- **Streaming is the real shape.** Anthropic's `tool_use` blocks arrive across many SSE frames; input JSON is streamed token-by-token. Waiting for the full response means latency is "however long the response is."
- **Decisions have to happen mid-stream.** The moment the LLM decides to call a tool, the loop must consult permission (s04) for an allow / deny / ask verdict; deny means we don't even need the rest. A one-shot API has nowhere to put that hook.
- **Vendor lock leaks the second the call site mentions Anthropic.** If s10's loop does `import "anthropic"`, Phase G's OpenAI / Bedrock support means rewriting the loop — and the loop has nothing to do with vendors. That's the Provider abstraction leaking through.

s01's interface shape happens to lock in all three. s05 swaps the shape itself.

## Solution

`Provider` is an interface with one method:

```go
type Provider interface {
    Stream(ctx context.Context, req Request) (Stream, error)
}
```

`Stream` is a pull-based iterator:

```go
type Stream interface {
    Next() (Event, error)   // returns io.EOF when the stream ends
    Close() error
}
```

`Event` is a tagged union: `EventText` (text delta), `EventToolUse` (a buffered, complete tool call), `EventReasoning` (extended-thinking chunk), `EventFinish` (terminal event + final Usage).

The Anthropic implementation `AnthropicProvider` does two things:

1. POST to `/v1/messages` with `"stream": true` in the body and `Accept: text/event-stream` in the headers.
2. An `*anthropicStream` reads SSE bytes — one `event: ...` line + one `data: {...JSON...}` line + a blank line — and translates each upstream event type (`message_start` / `content_block_start` / `content_block_delta` / `content_block_stop` / `message_delta` / `message_stop`) into our Event union.

The whole module is ~400 LOC, with 4 tests using `httptest` stub servers.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s05 Provider abstraction                                              │
│                                                                        │
│   var p Provider = NewAnthropicProvider(apiKey, model)                 │
│   stream, err := p.Stream(ctx, Request{...})    ──── POST /v1/messages │
│                                                       (stream=true)    │
│                                                                        │
│   Anthropic SSE bytes back ──→  *anthropicStream                       │
│                                                                        │
│   for {                              ┌─ message_start    → buffer input_tokens
│       ev, err := stream.Next()       ├─ content_block_start → record block kind
│       if errors.Is(err, io.EOF) {    ├─ content_block_delta:
│           break                      │     text_delta     → EventText
│       }                              │     input_json_delta → buffer in jsonAcc
│       switch ev.Type {               │     thinking_delta  → EventReasoning
│       case EventText:    ...         ├─ content_block_stop → tool_use block emits EventToolUse
│       case EventToolUse: ...         ├─ message_delta     → buffer output_tokens
│       case EventFinish:  ...         ├─ message_stop      → EventFinish (Usage)
│       }                              └─ (next Next call)  → io.EOF
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**Three load-bearing design points**:

1. **`Next()` returns `io.EOF` on clean end.** This is Go's idiomatic "stream finished" signal — same shape as `(*os.File).Read`, `(*bufio.Scanner).Scan`, channel-close patterns. The s06 / s10 / s14 loops will all be `for { ev, err := stream.Next(); if errors.Is(err, io.EOF) { break } }`, so this contract is fixed.
2. **tool_use input is buffered until `content_block_stop`.** Anthropic chunks input JSON across N `input_json_delta` frames; we accumulate them in `contentBlockBuffer.jsonAcc` and emit ONE `EventToolUse` at `content_block_stop`. Reason: a consumer can't dispatch a tool with half its arguments, and forcing every consumer to reimplement buffering would be duplicated effort.
3. **`EventFinish` and `io.EOF` are two separate `Next()` calls.** Finish carries data the caller needs (usage); EOF is the loop-exit signal — collapsing them would force a "did I get usage yet?" status flag in every consumer.

**Usage stitched across two SSE events**: `message_start` carries `input_tokens`; `message_delta` carries `output_tokens`. We accumulate them in `*anthropicStream.usage` and emit them in one shot at `message_stop`. Consumers see one `Usage`, vendor-agnostic.

## What Changed (vs. s01)

s01 did one blocking call; s05 keeps the same wire shape but stretches it into a stream:

```diff
 // s01: blocking, one round trip.
-resp, err := p.CreateMessage(ctx, req)
-for _, b := range resp.Content {
-    if b.Type == "text" { fmt.Println(b.Text) }
-}

+// s05: pull-based stream, react per event.
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

The HTTP request is nearly identical: same endpoint, same headers (`x-api-key`, `anthropic-version: 2023-06-01`, `Content-Type: application/json`), same JSON body — plus a `"stream": true` field and the `Accept: text/event-stream` header. The response Content-Type changes from `application/json` to `text/event-stream`.

Abstraction-boundary diff: s01's `Provider` interface signature was `CreateMessage(ctx, req) (*Resp, error)`; s05's is `Stream(ctx, req) (Stream, error)`. The latter is a strict superset — a trivial implementation could drain the stream into one response, so s01's capability is a strict subset of s05's. Each later session adds capability monotonically.

`main.go` deliberately writes `var p Provider = NewAnthropicProvider(...)` instead of `p := NewAnthropicProvider(...)` — to underline that nothing below this line knows what vendor it's talking to. When Phase G adds `OpenAIProvider`, only this single line changes.

## Try It

```bash
cd agents/s05-provider-iface

# Real streaming demo (needs ANTHROPIC_API_KEY):
export ANTHROPIC_API_KEY=sk-ant-...
go run . hello in three words

# 4 tests, all using httptest stub servers fed canned SSE bytes — no network.
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 4 test scenarios:

1. **text-only stream** — sequence of `EventText` events concatenates correctly.
2. **tool_use stream** — multiple `input_json_delta` frames assemble into one `EventToolUse`; name/id/input all parsed.
3. **reasoning stream** — `thinking_delta` decodes to `EventReasoning`.
4. **message_stop → EventFinish + io.EOF** — the terminal contract: the finish event arrives first, and the next `Next()` call must return `io.EOF`, with Usage joining input/output tokens across the two SSE events.

## Upstream Source Reading

s05 mirrors opencode's `packages/opencode/src/provider/provider.ts`. The whole file is 1792 lines — but most of the cognitive load is the 23-row `BUNDLED_PROVIDERS` map: each key is an npm package name, each value is a thunk that `import()`s that vendor's SDK on demand. This is the entry point of opencode's whole multi-provider design; every vendor abstraction unfolds from here.

```ts
// upstream:packages/opencode/src/provider/provider.ts L87-L117
type BundledSDK = {
  languageModel(modelId: string): LanguageModelV3
}

const BUNDLED_PROVIDERS: Record<string, () => Promise<(opts: any) => BundledSDK>> = {
  "@ai-sdk/amazon-bedrock": () => import("@ai-sdk/amazon-bedrock").then((m) => m.createAmazonBedrock),
  "@ai-sdk/anthropic": () => import("@ai-sdk/anthropic").then((m) => m.createAnthropic),
  "@ai-sdk/azure": () => import("@ai-sdk/azure").then((m) => m.createAzure),
  "@ai-sdk/google": () => import("@ai-sdk/google").then((m) => m.createGoogleGenerativeAI),
  "@ai-sdk/google-vertex": () => import("@ai-sdk/google-vertex").then((m) => m.createVertex),
  "@ai-sdk/google-vertex/anthropic": () =>
    import("@ai-sdk/google-vertex/anthropic").then((m) => m.createVertexAnthropic),
  "@ai-sdk/openai": () => import("@ai-sdk/openai").then((m) => m.createOpenAI),
  "@ai-sdk/openai-compatible": () => import("@ai-sdk/openai-compatible").then((m) => m.createOpenAICompatible),
  "@openrouter/ai-sdk-provider": () => import("@openrouter/ai-sdk-provider").then((m) => m.createOpenRouter),
  "@ai-sdk/xai": () => import("@ai-sdk/xai").then((m) => m.createXai),
  "@ai-sdk/mistral": () => import("@ai-sdk/mistral").then((m) => m.createMistral),
  "@ai-sdk/groq": () => import("@ai-sdk/groq").then((m) => m.createGroq),
  "@ai-sdk/deepinfra": () => import("@ai-sdk/deepinfra").then((m) => m.createDeepInfra),
  "@ai-sdk/cerebras": () => import("@ai-sdk/cerebras").then((m) => m.createCerebras),
  "@ai-sdk/cohere": () => import("@ai-sdk/cohere").then((m) => m.createCohere),
  "@ai-sdk/gateway": () => import("@ai-sdk/gateway").then((m) => m.createGateway),
  "@ai-sdk/togetherai": () => import("@ai-sdk/togetherai").then((m) => m.createTogetherAI),
  "@ai-sdk/perplexity": () => import("@ai-sdk/perplexity").then((m) => m.createPerplexity),
  "@ai-sdk/vercel": () => import("@ai-sdk/vercel").then((m) => m.createVercel),
  "@ai-sdk/alibaba": () => import("@ai-sdk/alibaba").then((m) => m.createAlibaba),
  "gitlab-ai-provider": () => import("gitlab-ai-provider").then((m) => m.createGitLab),
  "@ai-sdk/github-copilot": () =>
    import("@opencode-ai/core/github-copilot/copilot-provider").then((m) => m.createOpenaiCompatible),
  "venice-ai-sdk-provider": () => import("venice-ai-sdk-provider").then((m) => m.createVenice),
}
```

Line-by-line annotation (the load-bearing rows):

- **L87-L89 `BundledSDK`** — this is the *shape* opencode expects from every vendor SDK: a `languageModel(id)` method returning `LanguageModelV3` (the Vercel AI SDK's internal interface). Our Go translation IS the `Provider` interface itself — one method, `Stream(ctx, Request)`.
- **L91 `Record<string, () => Promise<factory>>`** — three layers of indirection: string key → thunk → thunk does `import()` → import resolves to a factory func → factory func takes options, returns BundledSDK. Go has no dynamic import, so we flatten to *constructors*: `NewAnthropicProvider(apiKey, model)` directly returns something that satisfies Provider. Phase G will have `NewOpenAIProvider`, `NewBedrockProvider` — different names, same signature.
- **L92-L93 Anthropic / Bedrock entries** — these are the rows s05 directly mirrors. What our `AnthropicProvider.Stream` does is what `createAnthropic(opts).languageModel(modelID)` does after the AI SDK's internals run — except we hand-roll the SSE parser instead of pulling in the SDK.
- **L99-L100 OpenAI entry** — Phase G's sibling. OpenAI's wire is `/v1/chat/completions` with different SSE event names (`data: [DONE]` instead of `message_stop`), but our Provider interface is unchanged — same `Request` in, same `Stream` out. That's the entire point of the interface.
- **L101-L116** — 22 other vendors. Note OpenRouter and Gateway are *meta* providers that route to underlying vendors. The Go translation either adds them as separate structs (if their wire differs) or wraps `OpenAIProvider` with a base-URL override (OpenRouter speaks the OpenAI-compatible API).

Permalinks:

- BUNDLED_PROVIDERS (L87-L117): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/provider/provider.ts#L87-L117>
- custom loaders (L119-L150): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/provider/provider.ts#L119-L150>

What we kept and what we dropped:

- **Kept** — Provider-as-interface shape, the BundledSDK "one method" contract (our `Stream`), the Anthropic wire shape byte-for-byte (request body, headers, SSE event types → our Event union).
- **Dropped (for now)** — 22 non-Anthropic vendors (Phase G adds OpenAI as the second; the rest as needed); `wrapSSE()`'s timeout cancellation (we use `context.Context` deadlines instead); custom loaders (the `L149+` per-vendor overrides — Azure/Vertex's special model selection); plugin-installed providers; `ProviderTransform`'s request rewriting.
- **Forward-compat** — Phase G's OpenAI is one new file (`provider_openai.go`), one new struct (`OpenAIProvider`), one new constructor (`NewOpenAIProvider`). s06 / s10 / s14 don't change a line. That's the proof this abstraction is *worth doing* — the cost of adding a vendor is O(1), not O(consumers).

Reading order for opencode's provider layer:

1. `packages/opencode/src/provider/provider.ts` L87-L117 — the `BUNDLED_PROVIDERS` map (this s05)
2. `packages/opencode/src/provider/provider.ts` L39-L85 — `wrapSSE()` timeout cancellation (we replace with ctx)
3. `packages/opencode/src/session/llm.ts` L100-L200 — how `streamText()` is called from the loop (s06)
4. `packages/opencode/src/provider/provider.ts` L149+ — custom loaders (Phase G)
5. `packages/opencode/src/session/processor.ts` L34-L150 — how Events become Parts and tools dispatch (s10)
