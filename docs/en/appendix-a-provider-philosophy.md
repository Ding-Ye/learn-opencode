---
title: "Appendix A · Provider abstraction philosophy"
chapter: 100
slug: appendix-a-provider-philosophy
est_read_min: 14
---

# Appendix A · Provider abstraction philosophy

> What this appendix teaches: why the **Provider** layer in s05 is an interface and not a concrete type, what the failure mode is that the abstraction prevents, how the TypeScript world (Vercel AI SDK + opencode's `BUNDLED_PROVIDERS`) solves the same problem, what Go's idiomatic translation of that pattern looks like, and what we lose at the seam — provider-specific options like Anthropic's prompt-caching headers or OpenAI's structured-output mode that don't fit through a one-size interface. This is the mental model that makes s05 / s06 / s10 read coherently; it is also reusable for any agent framework you build later.

---

## The failure mode being avoided

Hard-coding one provider's HTTP shape into your agent is a debt that comes due on a specific Tuesday: the day Anthropic deprecates `messages-2023-06-01` (or OpenAI flips ChatCompletions to a v2 schema, or Google moves Gemini from `generateContent` to `streamGenerateContent`, or your org has to add Bedrock for compliance reasons). On that Tuesday, every file in your codebase that mentions `claude-3-5-sonnet`, `https://api.anthropic.com/v1/messages`, `x-api-key`, `content_block_delta`, or `messages.start` is touched.

Concrete versions of this debt:

- **The string literal `"https://api.anthropic.com/v1/messages"` shows up in 14 files.** When the URL changes, your test fixtures, your retry wrapper, your usage parser, your auth header logic all need patches. None of them needed to know the URL — they only needed to know "give me a streaming response of typed Events".
- **The SSE parser is shaped to Anthropic's framing.** `messages.start` / `content_block_delta` / `message_stop` is one specific protocol. OpenAI's SSE uses `data: {choices:[{delta:{content:...}}]}` with a sentinel `data: [DONE]` line. If your loop reads `event.type === "content_block_delta"` directly, you can't even point at a different vendor without rewriting the consumer.
- **Tool-use shape leaks.** Anthropic emits `{type: "tool_use", id, name, input}` as a content block; OpenAI emits `{tool_calls: [{id, function: {name, arguments}}]}` as a top-level message field. If your registry dispatches off the Anthropic shape, the OpenAI translation has to happen in the wrong place — at every callsite — instead of once, at the seam.
- **Auth headers leak.** `x-api-key` (Anthropic) vs `Authorization: Bearer` (OpenAI) vs `aws-sigv4` (Bedrock) vs OAuth (Copilot) vs IAM (Vertex). If your retry/refresh logic reaches into the request to add headers, it implicitly knows about every provider.

The shared root cause of all four is **representation leak**: provider-specific data formats (URL, SSE framing, tool shape, headers) are visible to code that didn't need them. The fix is the standard one — put a seam where representation differs and let only the implementation behind the seam know the format.

The seam is the Provider interface. Behind it: one Anthropic implementation, one OpenAI implementation, one fake for tests. In front of it: every other module — Streaming Loop, Orchestrator, Usage accumulator — sees a uniform `Stream(ctx, Request) → Stream<Event>` shape. When the Tuesday-shaped failure arrives, you change exactly the implementation behind the seam.

## Vercel AI SDK's approach in TypeScript

The TypeScript ecosystem solves this with the Vercel AI SDK (`ai` on npm). Its central abstraction is `streamText({ model, system, messages, tools })`. The `model` parameter is a value, not a code path:

```ts
import { streamText } from "ai";
import { anthropic } from "@ai-sdk/anthropic";
import { openai } from "@ai-sdk/openai";

// the call site has no provider knowledge:
const { textStream, toolCalls } = await streamText({
  model: anthropic("claude-3-5-sonnet"),  // or openai("gpt-4o")
  system: "You are helpful.",
  messages: [{ role: "user", content: "hello" }],
  tools: { /* ... */ },
});
```

Two design decisions are doing all the work:

1. **`model` is a `LanguageModelV3` value, not a string and not a class to subclass.** `anthropic(...)` is a factory function that returns an object satisfying the `LanguageModelV3` interface (`doStream`, `doGenerate`, capability flags). The SDK never type-switches on which provider produced the value; it calls `model.doStream(prompt)` and trusts the implementation.
2. **All providers normalize to a single Event protocol.** Whether the wire format was Anthropic's `content_block_delta` or OpenAI's `data: {choices:[...]}`, by the time it crosses the seam it's a `LanguageModelV3StreamPart` — text-delta, tool-call-delta, tool-call, finish, error, etc. The framing difference is buried inside `@ai-sdk/openai`'s and `@ai-sdk/anthropic`'s respective adapters.

This is "provider as data, not as code path" — the call site holds a value of an interface type and dispatches through it. The cost is that anything truly provider-specific (Anthropic's prompt-caching headers, OpenAI's `response_format: {type: "json_schema"}`, Bedrock's guardrails) has to either fit into the interface (typically as `providerOptions: { anthropic?: {...}, openai?: {...} }`) or get punted to a side channel. We come back to this in section 5.

## opencode's BUNDLED_PROVIDERS map

opencode wraps the Vercel SDK with a registry that resolves a string provider ID to a factory function. The whole thing fits in one map:

```ts
// packages/opencode/src/provider/provider.ts L91–L117
const BUNDLED_PROVIDERS: Record<string, () => Promise<(opts: any) => BundledSDK>> = {
  "@ai-sdk/amazon-bedrock": () => import("@ai-sdk/amazon-bedrock").then((m) => m.createAmazonBedrock),
  "@ai-sdk/anthropic": () => import("@ai-sdk/anthropic").then((m) => m.createAnthropic),
  "@ai-sdk/azure": () => import("@ai-sdk/azure").then((m) => m.createAzure),
  "@ai-sdk/google": () => import("@ai-sdk/google").then((m) => m.createGoogleGenerativeAI),
  "@ai-sdk/google-vertex": () => import("@ai-sdk/google-vertex").then((m) => m.createVertex),
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
};
```

Three things to notice:

- **The values are lazy thunks** (`() => import(...)`). The `import(...)` is dynamic, so a user who never sets `provider: "groq"` never pays Groq's bundle weight. In Go we have no equivalent — every provider is compiled in unconditionally — but we also don't have a 25-provider bundle weight problem.
- **The keys are npm-package names, not friendly IDs.** That's because the same map is used both as "what to load" and "what to credit" in opencode's logging/auth. A friendlier `"anthropic" → loader` map is the obvious refactor; opencode chose self-documenting keys instead.
- **The factory returns an `(opts: any) => BundledSDK` thunk**, not the SDK itself. This second indirection is where API keys and per-call options get bound. In Go we'd represent this as a constructor: `func NewAnthropic(opts AnthropicOpts) Provider`.

The mechanics around the map are unremarkable: `provider.ts` resolves a `(providerID, modelID)` pair by looking up the loader, awaiting it, calling it with auth options, then asking the resulting SDK for `languageModel(modelID)`. The outer code holds that `LanguageModelV3` value and passes it to `streamText`.

## Go's idiomatic translation

Go has neither dynamic imports nor TypeScript's structural typing. The translation is:

1. **Define `Provider` as an interface in s05.**

   ```go
   type Provider interface {
       Stream(ctx context.Context, req Request) (Stream, error)
   }
   ```

2. **Define `Stream` and `Event` as interface + tagged union.**

   ```go
   type Stream interface {
       Next() (Event, error)   // returns ErrStreamDone when done
       Close() error
   }

   type Event struct {
       Type      EventType   // EventText | EventToolUse | EventFinish | …
       Text      string
       ToolUse   *ToolUse
       Reasoning string
       Usage     *Usage
   }
   ```

3. **Implement one provider per file.** s05 ships `AnthropicProvider`. A future addendum would add `OpenAIProvider` in `provider_openai.go`. Each constructor is a plain Go function that closes over its API key and base URL.

4. **Register constructors in a map at package init (or just at `main`).**

   ```go
   var Providers = map[string]func(opts ProviderOpts) Provider{
       "anthropic": NewAnthropicProvider,
       // "openai": NewOpenAIProvider,  // Phase G
   }
   ```

   In Go this map is built at compile time, not by `import()`. That's a tradeoff: simpler to read (no async loader), but every provider is in the binary whether you use it or not. For a teaching repo with one or two providers that's a non-issue; for a tool that wants to ship 25 providers it matters.

5. **Compose at `main`**: read config, look up the constructor by ID, build the Provider, hand it to the Orchestrator (s10). The Orchestrator's signature mentions only the interface — `func Run(ctx context.Context, p Provider, r *Registry, msg Message) ([]Message, error)`. It can't accidentally know about Anthropic.

The shape difference vs TypeScript:

- **No dynamic dispatch on string keys.** Go's static-dispatch interface is the static-dispatch equivalent of `LanguageModelV3`. The map is just for *building* the provider, not for *using* it.
- **No structural typing.** Anything claiming to be a `Provider` must explicitly satisfy the interface; that's enforced by the compiler. In TS, satisfying `LanguageModelV3` is enough — there's no `implements` keyword required.
- **No lazy imports.** All providers are linked in. We pay binary size to gain build-time guarantees.

The interface itself is small on purpose. Adding methods is expensive (every existing implementation has to grow). Adding fields to `Request` and `Event` is cheap (existing implementations ignore fields they don't recognize). When you're tempted to add `func (p Provider) AnthropicCacheHeader() string` — don't. Add an optional field to `Request`, document it as Anthropic-specific, and let other providers ignore it. That's exactly what the Vercel SDK does with `providerOptions`.

## What's lost: provider-specific options at compile time

The price of "uniform interface" is that genuinely-different provider features no longer have a typed, IDE-discoverable home. Three concrete examples:

**1. The `req.Tools` shape divergence.** Anthropic's tool format is `{name, description, input_schema}`. OpenAI's is `{type: "function", function: {name, description, parameters}}`. Google Gemini's is `{name, description, parameters}` with a function-declarations wrapper. In s05's interface, `Request.Tools []ToolSchema` carries a single shape (we picked Anthropic's, since that's our default). When `OpenAIProvider.Stream` is implemented, it has to *translate* `[]ToolSchema` into the OpenAI shape inside its body. There's no compile-time check that this translation handles every field; the Anthropic-shape `cache_control` field, for instance, has no OpenAI equivalent and gets dropped silently.

   The Vercel SDK handles this with a **canonical internal tool format** (`LanguageModelV3FunctionTool`) that every adapter translates *into*. opencode inherits that — the call site never sees provider-specific tool JSON. We simulate the same thing in Go: `Request.Tools` is the canonical form, every Provider impl translates outward.

**2. Optional reasoning param.** Anthropic's extended-thinking models accept `thinking: {type: "enabled", budget_tokens: 10000}` to enable reasoning blocks. OpenAI's `o1`/`o3` models reason silently by default (no client opt-in). Google Gemini has `thinkingBudget`. In a uniform `Request`, this becomes:

   - Either a **typed field** (`Request.ReasoningBudget int`) that some providers honor and others ignore — discoverable in the IDE, unreliable in semantics.
   - Or a **provider-options bag** (`Request.ProviderOptions map[string]json.RawMessage`) — type-erased, reliable, undiscoverable.

   The Vercel SDK chose the bag (`providerOptions: { anthropic: {...}, openai: {...} }`). For a Go teaching repo we recommend a hybrid: typed field for things ≥2 providers support, bag for anything genuinely vendor-specific.

**3. Anthropic's prompt-caching headers vs OpenAI's lack thereof.** Anthropic supports cached prefixes via `cache_control: {type: "ephemeral"}` blocks in the request body, plus a beta header `anthropic-beta: prompt-caching-2024-07-31`. OpenAI has no analogous user-facing API (caching is server-side and automatic). If `Request` exposes `EnablePromptCaching bool`, it's a lie on OpenAI. The honest API is per-message metadata — `Part.Metadata["cache"]` — that the Anthropic provider reads and the OpenAI provider ignores. That's ugly but truthful.

The general lesson: **the more providers you abstract over, the more your interface degrades into a least-common-denominator shape, and the more provider-specific power gets shoved into untyped escape hatches.** The Vercel SDK accepts this trade explicitly with `providerOptions`. opencode inherits the trade. learn-opencode's s05 ducks the question by being Anthropic-only at compile time; the moment we add a second provider in a future addendum, we'll need the same escape hatch.

## What's gained: testability via fake Provider

The flip side of all that pain is the cleanest thing in the agent: **a fake provider for tests**. Because every consumer of LLM output speaks the `Provider` / `Stream` / `Event` interface, test code can substitute a hand-scripted Provider that emits a predetermined Event sequence, with zero HTTP, zero API keys, zero rate limits, zero flakiness.

s06 ships exactly this in `agents/s06-streaming-loop/fake_provider.go`. The shape is:

```go
type FakeProvider struct {
    Events [][]Event   // one slice per Stream() call
    call   int
}

func (f *FakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
    if f.call >= len(f.Events) {
        return nil, fmt.Errorf("fake provider exhausted")
    }
    s := &fakeStream{events: f.Events[f.call]}
    f.call++
    return s, nil
}
```

Tests then build the exact event sequence they want to exercise:

```go
fp := &FakeProvider{Events: [][]Event{
    {
        {Type: EventText, Text: "I'll read the file."},
        {Type: EventToolUse, ToolUse: &ToolUse{ID: "t1", Name: "read", Input: ...}},
        {Type: EventFinish, Usage: &Usage{InputTokens: 100, OutputTokens: 30}},
    },
    {
        {Type: EventText, Text: "Here is what I found."},
        {Type: EventFinish, Usage: &Usage{InputTokens: 150, OutputTokens: 20}},
    },
}}
```

s10's `loop_test.go` builds on this: it scripts a 2-iteration conversation (assistant calls a tool, gets a result, then completes), runs the orchestrator against the fake, and asserts the final message slice has exactly the expected parts. No network, no API key, deterministic, runs in under a millisecond.

Two things this buys us beyond just "tests are fast":

- **Adversarial scenarios are easy.** The fake can emit `EventToolUse` with a permission-denied target, then assert the orchestrator returns `IsError: true` in the result Part (not a panic, not a crash). The fake can emit a `context_length_exceeded` mid-stream, then assert s14's classifier returns `ShouldCompact == true`. The fake can refuse to terminate, then assert `MaxIterations` actually caps the loop.
- **The interface gets exercised, not just the implementation.** Every test that runs against `FakeProvider` is a guarantee that the orchestrator (and loop, and registry, and permission, and usage) speak the interface — not the Anthropic implementation. The day `OpenAIProvider` is added, all of those tests still pass without modification, because they never depended on Anthropic in the first place.

This is the pragmatic payoff of section 1's seam discipline: it isn't only about surviving the day Anthropic deprecates an endpoint. It's about being able to test the agent at all, without paying for tokens, without flake, without mocking HTTP. The interface-driven design pays dividends on every test run, every CI build, every refactor — not just on the rare wire-format-migration day.
