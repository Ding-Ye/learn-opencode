---
title: "s01 · Minimum agent loop"
chapter: 1
slug: s01-minimum-loop
est_read_min: 8
---

# s01 · Minimum agent loop

> What this teaches: the smallest call site that talks to an LLM. We're not building an agent yet — we're building the *thing the agent calls*. Without this primitive nailed down, every later session would inherit the wrong wire-format assumptions.

---

## Problem

opencode is a 159K-star coding agent that streams responses, dispatches tools, persists sessions, evaluates permissions, and routes between 25+ LLM providers. Reading its 183K-LOC core from the top is a great way to drown.

We need a one-page anchor: send a message to Anthropic, get a reply, print it. No streaming. No tools. No SQLite. No retries. If we get this shape wrong, every later session — `s02 message-parts`, `s05 provider-iface`, `s06 streaming-loop` — has to walk back and refactor it.

## Solution

The whole thing is two interfaces and one HTTP call:

1. **`Provider` is an interface** (not a struct) so when `s05 provider-iface` adds OpenAI, the call site at `main.go` doesn't change. This mirrors opencode's `BUNDLED_PROVIDERS` map (`packages/opencode/src/provider/provider.ts#L91-L117`).
2. **`Message`/`ContentBlock` use the Anthropic wire format directly** rather than a translated internal type. opencode pays a hidden cost for its `@ai-sdk` translation layer; we pay nothing because Go has no equivalent ecosystem and inventing one in s01 would be premature.
3. **`AnthropicProvider` carries an overridable `baseURL`** — only `withBaseURL` (a test helper) sets it. This is the trick that makes `provider_test.go` work with `httptest` and never call the real API.

## How It Works

```
┌────────────────────────────────────────────────────────────┐
│  s01 minimum loop                                          │
│                                                            │
│   main.go                                                  │
│      │                                                     │
│      │  builds CreateMessageRequest{System, Messages}      │
│      ▼                                                     │
│   Provider.CreateMessage(ctx, req) ────► Anthropic /v1/    │
│      │                                   messages          │
│      │  HTTP 200, JSON                                     │
│      ▼                                                     │
│   *CreateMessageResponse                                   │
│      ├─ .Content[0].Text  ──► fmt.Println                  │
│      └─ .Usage           ───► fmt.Fprintf(os.Stderr, …)    │
└────────────────────────────────────────────────────────────┘
```

The 50 lines that do the work are in `provider.go`:

```go
type Provider interface {
    CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error)
}

func (a *AnthropicProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
    if req.Model == "" { req.Model = a.model }
    if req.MaxTokens == 0 { req.MaxTokens = 4096 }

    body, _ := json.Marshal(req)
    httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL, bytes.NewReader(body))
    httpReq.Header.Set("x-api-key", a.apiKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")

    resp, err := a.client.Do(httpReq)
    // ... read, check status, json.Unmarshal into CreateMessageResponse
}
```

**Three non-obvious points**:

1. **Default-only-if-zero pattern** — `req.Model` and `req.MaxTokens` are filled from the provider's own defaults *only* when caller left them empty. This lets `main.go` stay terse but still allows per-call overrides (verified in `TestAnthropicProviderModelOverride`).
2. **Status check is `/100 != 2`** rather than `== 200` — Anthropic returns 200 today but might return 201 or 206 for streaming endpoints later, and we want s01's pattern to survive that.
3. **No retry, no backoff** — both belong in `s14-cost-and-recovery`. Trying to be clever here forces every later session to either inherit the policy or carve it out.

## What Changed (vs. baseline)

This is the first session — the whole repo is the diff. Every later session has a "What Changed" section here.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimum-loop

# default model, default system prompt
go run . hello in three words

# tweak model + system
go run . -model claude-haiku-4-5 -system "Reply in haiku" "what is HTTP"

# tests use httptest, no API call needed
go test -count=1 ./...
```

## Upstream Source Reading

The mechanism this s01 mirrors lives in opencode's `packages/opencode/src/session/llm.ts`:

```ts
// upstream:packages/opencode/src/session/llm.ts#L35-L120 (excerpt; ai-sdk wrapper)
export const Service = Effect.gen(function* () {
  const provider = yield* Provider.Service
  return {
    stream: (input) => Effect.gen(function* () {
      const model = yield* provider.model(input.providerID, input.modelID)
      return streamText({
        model,
        system: input.system,
        messages: input.messages,
        tools: input.tools,
        toolChoice: input.toolChoice,
        // ... + retry, abort, telemetry
      })
    })
  }
})
```

opencode's version threads through `Effect.gen` (typed-effect runtime), pulls a `Provider` service from a Layer, and calls Vercel AI SDK's `streamText`. Our s01 strips all of that:

- No Effect — Go uses plain `func` and `error`.
- No `streamText` — we call `messages.create` (non-streaming), promoted in s06.
- No tools — promoted in s03 / s10.
- No abort signal — `context.Context` carries it implicitly when we add streaming.

`packages/opencode/src/provider/provider.ts#L87-L150` is where opencode resolves a `(providerID, modelID)` pair to a concrete `LanguageModelV3`. The map of `BUNDLED_PROVIDERS` (line 91) is the philosophical ancestor of our future Phase G `provider_openai.go` and `provider_anthropic.go`.

Reading order for opencode's LLM layer:
1. `packages/opencode/src/provider/provider.ts` lines 1–150 — interface + provider map
2. `packages/opencode/src/session/llm.ts` lines 1–120 — the streamText invocation
3. `packages/opencode/src/session/processor.ts` lines 34–150 — the loop we'll build in `s10-tool-loop`

Don't read further yet — s02–s10 each take you deeper.
