---
title: "s06 · Streaming loop"
chapter: 6
slug: s06-streaming-loop
est_read_min: 12
---

# s06 · Streaming loop

> What this teaches: write the *consumer* of s05's `Provider.Stream`. A `Loop` aggregates streaming Events, one at a time, into a `Message` of `Parts` — N adjacent text deltas collapse into one `TextPart`; a buffered tool_use becomes one `ToolUsePart`; reasoning chunks fold into one `ReasoningPart`; `EventFinish` records Usage on the Message. This is the smallest *bridge* a streaming agent needs to replace the "wait for full response, then process" pattern.

---

## Problem

s05 upgraded the LLM call from "one blocking response" to "pull-based stream of events." But what comes next? The code holding that stream — what shape does it have to produce so the rest of the agent can use it?

- **Consumers can't operate on Events directly.** `Event` is the *wire-layer* abstraction — "what was the next SSE frame." But s07 needs to persist `Message` of `Parts`; s10's tool loop needs to know "how many tool_use Parts did this assistant message contain?"; s14's cost tracking needs `Message.Usage`. Every consumer needs the *aggregated* shape.
- **The aggregation rules aren't trivial.** A prose paragraph is N adjacent text deltas — those MUST collapse into one `TextPart` or s07 will store N rows per assistant turn (instead of 1) with no row boundary corresponding to "one complete utterance." But if a tool_use lands in the middle, the text after it MUST start a *fresh* `TextPart` because that's a different semantic boundary.
- **Letting every consumer reimplement the aggregation** duplicates code, and each callsite gets a chance to get it wrong (forget reasoning, forget finish, forget abort).
- **Aborts can't wait for the stream to finish.** User hits Ctrl-C — must stop NOW, not silently consume and discard the remaining events. That requires a `ctx.Err()` check between every `Next()`.

s06's job is to write *that* one consumer: `Loop`. Its responsibility ends there — no tool dispatch (s10), no permission check (s04 / s10), no persistence (s07), no retry (s14). One mechanism per session, no rewrites of the streaming layer.

## Solution

`Loop` is one struct and one method:

```go
type Loop struct { Provider Provider }

func (l *Loop) Consume(ctx context.Context, req Request) (*Message, error)
```

What `Consume` does:

1. `stream, err := l.Provider.Stream(ctx, req)`; bail on error.
2. `defer stream.Close()`.
3. In a `for` loop, `stream.Next()` until `io.EOF` returns `(*Message, nil)`.
4. Check `ctx.Err()` before each `Next()` — cancel returns `context.Canceled` immediately, no half-built Message.
5. Switch on `Event.Type`, append/extend `msg.Parts` per the aggregation rules:
   - `EventText` — if the last Part is `PartText`, extend it; otherwise append a new `TextPart`.
   - `EventToolUse` — always append a new `ToolUsePart` (input was already buffered by the s05 Provider). If `Name` is empty, return error (pointing at a Provider impl bug).
   - `EventReasoning` — same extend / append rule as text, for `PartReasoning`.
   - `EventFinish` — copy `Usage` to `msg.Usage`. The next `Next()` should return `io.EOF`.
6. Infer `StopReason`: last Part is `PartToolUse` → `"tool_use"` (s10's loop knows to re-ask), else `"end_turn"`.

The whole module is ~500 LOC: provider/parts/loop ~370 lines + ~130 lines of fake provider + tests. 4 tests, all using `fakeProvider`, no network.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s06 Loop.Consume                                                       │
│                                                                        │
│   loop := &Loop{Provider: ...}                                          │
│   msg, err := loop.Consume(ctx, Request{...})                           │
│                                                                        │
│   stream, _ := provider.Stream(ctx, req)                                │
│   defer stream.Close()                                                  │
│   msg := &Message{Role: RoleAssistant}                                  │
│   trailing := PartUnknown                                               │
│                                                                        │
│   for {                                                                │
│     if ctx.Err() != nil { return nil, ctx.Err() }   ← abort hatch     │
│     ev, err := stream.Next()                                            │
│     if errors.Is(err, io.EOF) { return msg, nil }   ← clean end       │
│                                                                        │
│     switch ev.Type {                                                    │
│     case EventText:                                                     │
│       if trailing == PartText { extend last TextPart }                  │
│       else                    { append new  TextPart  }                 │
│     case EventToolUse:        { append new  ToolUsePart (full input) } │
│       if ev.ToolUse.Name == "" → return error "tool name"               │
│     case EventReasoning:                                                │
│       if trailing == PartReasoning { extend } else { append new }       │
│     case EventFinish:         { msg.Usage = ev.Usage; infer StopReason }│
│     }                                                                  │
│     trailing = <kind we just appended>                                  │
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**Four load-bearing design points**:

1. **Adjacent-same-kind collapses; cross-kind breaks the run.** N text deltas fold into 1 `TextPart`; if a tool_use lands between two text deltas, the text after MUST start a fresh `TextPart`. This is the foundation of s10's "what did the message end with → dispatch a tool or finish the loop" decision — incorrectly concatenating loses the semantic boundary.
2. **`ctx.Err()` is checked before every `Next()`.** User hits Ctrl-C → ctx canceled → Loop returns `context.Canceled` immediately, NOT a half-built Message. A half-built Message is dangerous — callers will be tempted to "use what we got" and then s07 will persist a half-message to SQLite.
3. **`EventToolUse` missing `Name` fails immediately.** The Provider impl guarantees buffering before emitting `EventToolUse`; an empty Name is a bug. Failing loudly at the Loop layer points the error at the Provider; letting it through to s10's dispatcher fails as `unknown tool ""` with no clue which layer broke.
4. **`EventFinish` → `io.EOF` is two `Next()` calls.** Inherited verbatim from the s05 contract — Finish carries Usage, EOF is the loop-exit signal. Loop does NOT break after Finish; it loops once more to see EOF.

**Why Loop is ~150 lines**: because it only does aggregation. Every other concern is pushed to a later session — s10 adds dispatch, s07 adds persistence, s14 adds retry. This is the *physical* expression of incremental teaching: each session genuinely touches one file.

## What Changed (vs. s05)

s05's `main.go` demo printed each Event directly inside the `for` loop:

```diff
 // s05: print Event straight to screen — demo that Stream is pullable.
-stream, _ := p.Stream(ctx, req)
-defer stream.Close()
-for {
-    ev, err := stream.Next()
-    if errors.Is(err, io.EOF) { break }
-    switch ev.Type {
-    case EventText:    fmt.Print(ev.Text)
-    case EventToolUse: fmt.Printf("[tool_use] %s\n", ev.ToolUse.Name)
-    case EventFinish:  fmt.Printf("[tokens: %d/%d]\n", ev.Usage.InputTokens, ev.Usage.OutputTokens)
-    }
-}

+// s06: aggregate Events into a Message of Parts — turn the stream into something persistable, dispatchable, inspectable.
+loop := &Loop{Provider: p}
+msg, err := loop.Consume(ctx, req)
+if err != nil { return err }
+// msg.Parts is what s07 persists, what s10 dispatches tools from, what s14 bills Usage from.
+for _, part := range msg.Parts {
+    switch part.Kind {
+    case PartText:    fmt.Println(part.Text.Text)
+    case PartToolUse: dispatch(part.ToolUse)        // s10 lands here
+    }
+}
```

The Provider interface itself didn't change a line — that's proof s05's "Stream is the abstraction" decision was right. s06 adds a *consumer*, not a new producer.

Abstraction-boundary diff: before s06 there was no *consumer layer* concept — s05's main loop was an ad-hoc print snippet. s06 promotes that snippet to `Loop`, from demo-grade to production-grade (error handling, cancel, validation, structured output). s10's tool loop will *reuse* this `Consume`, wrapping it in an outer "dispatch tool + append result as user message + call Provider again" cycle.

A small detail to flag: `provider.go`'s `ToolUseEvent` (wire layer) and `parts.go`'s `ToolUsePart` (Part-variant after assembly) are two distinct structs. Their fields nearly overlap, but their semantics differ — one is "what the wire delivered just now," the other is "what I assembled into the Message." s10 will write the reverse translation (turn `ToolResultPart` into a wire-shape `tool_result` ContentBlock).

## Try It

```bash
cd agents/s06-streaming-loop

# Demo (deterministic, no network):
go run .

# 4 tests, all using fakeProvider with scripted Events; no network:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 4 test scenarios:

1. **text-only stream** — 3 EventText deltas collapse into 1 `TextPart`; Usage correct; StopReason inferred as `"end_turn"`.
2. **interleaved tool_use + text** — text + tool_use + text three Events become *3 Parts* (NOT 2 concatenated text + 1 tool_use). This is the foundation s10 builds on.
3. **AbortContext mid-stream** — `ctx.Cancel()` fires before the second Event, `Consume` returns `context.Canceled`, Message is `nil` (not half-built).
4. **malformed Event** — `EventToolUse` missing Name → Loop fails fast, error message contains "tool name", no half-built Message returned.

## Upstream Source Reading

s06 mirrors opencode's `packages/opencode/src/session/llm.ts`. The whole file is 469 lines; s06 cares about L100-L200 — the *prep* before `Provider.Stream` is called: composing the system prompt, merging options, running plugin hooks. We *cut all of it* (because s06 teaches the *consumer*, not the *prep*), keeping only the aggregation layer.

```ts
// upstream:packages/opencode/src/session/llm.ts L100-L143

// TODO: move this to a proper hook
const isOpenaiOauth = item.id === "openai" && info?.type === "oauth"

const system: string[] = []
system.push(
  [
    // use agent prompt otherwise provider prompt
    ...(input.agent.prompt ? [input.agent.prompt] : SystemPrompt.provider(input.model)),
    // any custom prompt passed into this call
    ...input.system,
    // any custom prompt from last user message
    ...(input.user.system ? [input.user.system] : []),
  ]
    .filter((x) => x)
    .join("\n"),
)

const header = system[0]
yield* plugin.trigger(
  "experimental.chat.system.transform",
  { sessionID: input.sessionID, model: input.model },
  { system },
)
// rejoin to maintain 2-part structure for caching if header unchanged
if (system.length > 2 && system[0] === header) {
  const rest = system.slice(1)
  system.length = 0
  system.push(header, rest.join("\n"))
}

const variant =
  !input.small && input.model.variants && input.user.model.variant
    ? input.model.variants[input.user.model.variant]
    : {}
const base = input.small
  ? ProviderTransform.smallOptions(input.model)
  : ProviderTransform.options({
      model: input.model,
      sessionID: input.sessionID,
      providerOptions: item.options,
    })
const options = mergeOptions(mergeOptions(mergeOptions(base, input.model.options), input.agent.options), variant)
```

Line-by-line annotation (the load-bearing rows):

- **L102-L114 system prompt assembly** — 4 sources: `agent.prompt` (introduced in s09), provider default (per-vendor default prompt), this-call system override, the user's last-message-attached system. `filter((x) => x).join("\n")` strips empties before joining. s06's `Request.System` is a single string — we *don't compose*. We need this layered shape only when s09's agent registry lands.
- **L116-L121 plugin hook `experimental.chat.system.transform`** — lets a user-installed plugin rewrite the system-prompt array before send. s06 has no plugin layer.
- **L122-L127 prompt-caching trick** — Anthropic's prompt cache only honors the first system block. opencode pins the header and folds all subsequent prompts into block #2 so multi-turn cache hits. s14's cost chapter may revisit.
- **L129-L132 variants** — one model id (`claude-sonnet-4-5`) can have high-effort / low-effort variants (different temperature etc.). s06's Request takes a flat `Model string` with no variant axis.
- **L133-L139 ProviderTransform.options** — returns the per-vendor options blob (Anthropic gets `{anthropic: {...}}`, OpenAI gets `{openai: {...}}`). s06's Request has no `Options` field — s05's interface deliberately drops vendor-specific knobs to keep the cross-vendor surface minimal. Phase G adds them back as `AnthropicOptions` / `OpenAIOptions` on the concrete provider.
- **L140 4-layer deep merge** — `base → model defaults → agent overrides → variant`, each layer beating the previous. s06's Request is a flat 6-field struct. The mental upgrade you should make here: this 4-layer merge is what `Provider.Stream` *abstracts away* from the consumer. The Loop has no idea what knobs were dialed; it only cares that Events come back. That IS the abstraction's value.

Permalinks:

- streamText prep (L100-L200): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L100-L200>
- streamText actual invocation (L336-L415): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L336-L415>
- AsyncIterable → Stream wrapper (L418-L432): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L418-L432>

What we kept, what we cut:

- **Kept** — the consumer layer's shape: pull Events from a `Stream`, aggregate into Parts, record Usage, infer StopReason. The aggregation rules match opencode's processor.ts reducer behavior (adjacent text folds, tool_use doesn't fold, reasoning folds).
- **Cut for now** — system prompt assembly (s09), provider options merging (use vendor defaults), plugin hooks (no plugin layer), variants (one Model string), tool dispatch (s10), permission check (s10), snapshot/diff (s07/s10), retry (s14), persistence (s07).
- **Forward-compat** — s10's tool loop *reuses* `Loop.Consume`, wrapping it in a "dispatch tool + append result as user message + call Provider again" outer cycle. s06 doesn't change a line. s07 adds persistence by feeding `Consume`'s returned Message into SQLite. s14 adds retry by wrapping `Consume` in a retry wrapper. The Loop signature `(ctx, req) → (*Message, error)` is side-effect-free, retry-safe.

Reading order for opencode's session layer:

1. `packages/opencode/src/session/llm.ts` L100-L143 — streamText prep (what s06 cuts)
2. `packages/opencode/src/session/llm.ts` L336-L415 — the actual streamText() call (s05 hand-rolled the SSE)
3. `packages/opencode/src/session/processor.ts` L34-L150 — Event reducer, where s10 reuses our Loop and adds dispatch
4. `packages/opencode/src/session/llm.ts` L418-L432 — AsyncIterable → Stream (our Stream interface is the equivalent)
5. `packages/opencode/src/session/processor.ts` (tool dispatch) — s10's "dispatch + feedback"
