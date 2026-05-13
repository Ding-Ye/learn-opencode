---
title: "s14 · Cost & error recovery"
chapter: 14
slug: s14-cost-and-recovery
est_read_min: 11
---

# s14 · Cost & error recovery

> What this chapter teaches: every LLM request returns a token tally and may fail in three meaningfully different ways. s14 builds the two operational layers that turn those facts into a working agent: a `Usage` accumulator with `Add`/`TotalCost` so the session row always reflects the WHOLE conversation's spend, and a `WithRetry` wrapper plus a four-error taxonomy (`APIError` / `AuthError` / `ContextOverflowError` / `AbortedError`) so a 429 backs off, an auth error bails immediately, a context overflow signals compaction, and a Ctrl-C beats the backoff sleep.

---

## Problem

s07 gave us a session row to persist; s10 wired the streaming tool loop. Two operational gaps remain that any real agent has to close before users will trust it:

- **Cost is invisible.** Every Anthropic request returns a `usage` object with five integers (input, output, reasoning, cache.read, cache.write). If we don't accumulate them onto the session row, we can't answer "what did this conversation cost me?" — and worse, we can't surface a running tally so the user can stop a runaway loop before it drains a budget.
- **Failures are not all the same.** A 429 (rate limit) is transient: wait and retry. A 503 (gateway timeout) is transient: wait and retry. A 401 (bad API key) is permanent: re-trying the same credential will fail the same way. A `context_length_exceeded` error means the conversation got too long: the recovery is summarize-and-resend, NOT retry. A user pressing Ctrl-C means stop now, NOT after the next backoff. An agent that catches every error with the same `if err != nil { retry() }` will burn budget on auth errors and ignore the user's cancellation.

Concrete pains the agent without these layers hits:

- A long-running loop encounters a 429 and crashes; the user has to re-prompt manually. Retry-with-backoff would have recovered transparently.
- An expired API key triggers a retry storm — three back-to-back 401s in 600ms — that some providers count as suspicious activity.
- The conversation hits 200k tokens, the next request returns `context_length_exceeded`, and the loop retries it three times before giving up. Each retry was guaranteed to fail with the same payload; the only fix is compaction.
- The user hits Ctrl-C during a 5-second backoff sleep. They expect an immediate exit; they get a hang while the timer expires.

opencode's answer is to **classify errors at the seam between transport and business logic, retry only the retryable ones, and signal compaction for the overflow case** — plus accumulate every request's `usage` onto the session row so the running cost is always one column read away.

s14 builds the cost-and-recovery seam. It does NOT build:

- The compaction routine itself (the ~400-LOC `Session.compact()` that summarizes old messages — out of scope; s14 only ships the SIGNAL `ShouldCompact(err) == true`).
- Decimal-precision billing (we use float64; production opencode uses Decimal.js; for a teaching repo float64 is fine to ~15 sig digits).
- Live pricing from a provider catalog (we hardcode MOCK constants for two models; real opencode reads rates from the AI SDK at request time).
- The full `@ai-sdk` APIError shape (responseHeaders, requestBodyValues, the `isRetryable` flag baked into the error itself); we keep StatusCode + Body and let `IsRetryable` infer from status code.

## Solution

A struct, three functions, four error types, and one classifier per recovery path:

```go
type Usage struct {
    InputTokens, OutputTokens, ReasoningTokens int
    CacheReadTokens, CacheWriteTokens          int
}
func (u *Usage) Add(other Usage)
func (u Usage) TotalCost(pricing Pricing) float64

type APIError struct { StatusCode int; Body string }   // transport / server
type AuthError struct { Provider string }              // bad credential
type ContextOverflowError struct { CurrentTokens, ModelLimit int }
type AbortedError struct{}
func IsRetryable(err error) bool                        // classifier 1
func ShouldCompact(err error) bool                      // classifier 2

type RetryPolicy struct { MaxAttempts int; BaseBackoff, MaxBackoff time.Duration }
func WithRetry(ctx context.Context, p RetryPolicy, op func() error) error
```

What each does:

- **`Usage.Add`**: adds `other` into the receiver in place. Used after every Provider.Stream completes — usage from this turn accumulates onto the running session-wide Usage so the persisted row reflects the WHOLE conversation, not just the last turn.
- **`Usage.TotalCost`**: dollars at the given Pricing rates. Reasoning tokens are billed at the OUTPUT rate (mirrors Anthropic's policy), so we fold reasoning into the output term rather than carrying a separate `ReasoningPerMTok` field.
- **`IsRetryable`**: true ONLY for `*APIError` with status 429 or status >= 500. Everything else (AuthError, ContextOverflowError, AbortedError, 4xx other than 429, plain stdlib errors) returns false. Re-trying an AuthError would be a footgun; re-trying a ContextOverflowError is guaranteed to fail.
- **`ShouldCompact`**: true ONLY for `*ContextOverflowError`. The recovery for context overflow is fundamentally different from retry — it MUTATES the request payload (drops or summarizes messages). Two recoveries, two predicates; they don't share a return value.
- **`WithRetry`**: runs op, classifies its error, sleeps with exponential backoff (capped at `MaxBackoff`), retries up to `MaxAttempts`. Three load-bearing rules: classify before retrying, respect ctx.Done() during sleeps, return the LAST error (not the first) if all attempts fail.

**Why two classifiers, not one tri-state enum**: we could write `Classify(err) → Retry | Compact | Fatal`, but the call sites are different. Retry lives inside `WithRetry`; compaction lives in s10's outer loop after `WithRetry` returns. A function with a single use site at each layer reads more clearly than a single function with three branches consumed in two places.

**Why the Usage struct flattens cache**: upstream nests `tokens.cache.{read,write}` to mirror Anthropic's API response shape. In Go there's no idiomatic reason to nest two integers; flattening to `CacheReadTokens` + `CacheWriteTokens` reads cleaner and matches Go's preference for shallow structs. The math (TotalCost) is identical.

**Why MOCK pricing constants**: real opencode pulls live rates from the AI SDK's provider catalog at request time (rates change; new models appear). For a teaching repo, hardcoded constants keep the demo deterministic and the test stable. The `// MOCK` comment on each is a load-bearing warning to anyone tempted to copy them into a billing pipeline.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s14 cost + recovery                                                   │
│                                                                        │
│  ── cost ─────────────────────────────────────────────────             │
│   Usage{InputTokens: 1200, OutputTokens: 350,                          │
│         ReasoningTokens: 80, CacheReadTokens: 10000,                   │
│         CacheWriteTokens: 200}                                         │
│        ↓ Add(turn2)                                                    │
│   Usage{... accumulated five-int totals ...}                           │
│        ↓ TotalCost(PricingClaudeSonnet4_5)                             │
│   $0.026700  (reasoning billed at OUTPUT rate)                         │
│                                                                        │
│  ── recovery ─────────────────────────────────────────────             │
│   WithRetry(ctx, p, op):                                               │
│     for attempt = 0 .. p.MaxAttempts-1:                                │
│       if ctx.Err() != nil { return ctx.Err() }     ← cancel wins       │
│       err = op()                                                       │
│       if err == nil { return nil }                                     │
│       if !IsRetryable(err) { return err }          ← bail on auth/...  │
│       if attempt == last { break }                                     │
│       select {                                                         │
│         case <-ctx.Done(): return ctx.Err()        ← cancel wins       │
│         case <-time.After(backoff):                ← sleep             │
│       }                                                                │
│       backoff = min(backoff*2, p.MaxBackoff)       ← exponential cap   │
│     return lastErr                                 ← last, not first   │
│                                                                        │
│   IsRetryable:    APIError{429} | APIError{>=500}  → true              │
│                   everything else                  → false             │
│   ShouldCompact:  ContextOverflowError             → true              │
│                   everything else                  → false             │
│                                                                        │
│  ── outer (s10's loop) ─────────────────────────────                   │
│   err := WithRetry(ctx, policy, func() error {                         │
│       stream, err := provider.Stream(ctx, req)                         │
│       if err != nil { return err }                                     │
│       sessionUsage.Add(consume(stream))            ← accumulate        │
│       return nil                                                       │
│   })                                                                   │
│   switch {                                                             │
│   case ShouldCompact(err):  compactAndRetry(...)   ← context overflow  │
│   case errors.As(err, &authErr):  promptReauth()   ← auth              │
│   case err != nil:          surfaceToUser(err)     ← give up           │
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**Five load-bearing decisions**:

1. **Classify before retrying.** `WithRetry` calls `IsRetryable(err)` before sleeping. AuthError and ContextOverflowError never sleep; they return on attempt 0. This is a SAFETY property (not just an optimisation) because retrying an AuthError can trigger account lockout, and retrying a ContextOverflowError wastes the budget on a guaranteed-failing payload.
2. **ctx beats backoff.** The select in the sleep loop has TWO cases: timer fire, and ctx.Done(). If both are ready, Go's select picks pseudo-randomly — but ctx wins in practice because once it's fired, the cancellation surface is checked at the top of every iteration too. Pinned by `TestWithRetryRespectsContextCancel`.
3. **Last error wins.** When all attempts fail, return the LAST `err`, not the first. The most recent server response is usually the most informative (a transient 503 followed by a permanent 503 should surface the permanent one). Mirrors upstream's `lastError` accumulator pattern.
4. **Two classifiers, not one switch.** `IsRetryable` and `ShouldCompact` are deliberately separate. Conflating them invites bugs where a context-overflow error accidentally retries before someone notices the payload didn't change.
5. **Reasoning bills like output.** `TotalCost` adds `OutputTokens + ReasoningTokens` and multiplies by the output rate. Anthropic charges reasoning at the same rate as output; folding them into one term matches the bill and avoids carrying a separate `ReasoningPerMTok` field that would always equal `OutputPerMTok` anyway.

**Why ~400 LOC (including tests)**: the work is small. Usage is five ints + an `Add` + a multiplication. Errors are four types + two classifier functions. Retry is a for-loop with a select. The 5 tests probe every branch. No goroutines, no channels (the timer is the only async surface), no I/O — Go's standard library is enough.

## What Changed (vs. s10/s11)

s10 wired the streaming tool loop: `for !done { stream → dispatch tools → append results → call provider again }`. s11 added skill discovery on top of s08's config layer. s14 wraps a NEW operational layer around s10's loop:

```diff
 // s10: bare provider call inside the loop.
 stream, err := provider.Stream(ctx, req)
 if err != nil {
-    return fmt.Errorf("provider stream: %w", err)
+    return fmt.Errorf("provider stream: %w", err)  // unwrapped, single attempt
 }

+// s14: wrap each turn's provider call in WithRetry; accumulate usage.
+var turnUsage Usage
+err := WithRetry(ctx, DefaultRetryPolicy(), func() error {
+    stream, err := provider.Stream(ctx, req)
+    if err != nil {
+        return err
+    }
+    turnUsage = consume(stream)  // capture per-turn usage from stream events
+    return nil
+})
+session.Usage.Add(turnUsage)
+session.Cost = session.Usage.TotalCost(modelPricing)
+
+switch {
+case ShouldCompact(err):
+    // Drop or summarize old messages, then re-issue the request.
+    req = compact(req)
+    continue
+case errors.As(err, &authErr):
+    return fmt.Errorf("re-authenticate: %w", err)
+case err != nil:
+    return err
+}
```

What's load-bearing about the diff: s10's loop didn't change shape (same outer for-loop, same provider.Stream call). s14 ADDED a wrapper (WithRetry) and a post-loop usage accumulation. The decoupling is deliberate — s10's contract ("turn → stream → tools → repeat") is independent from s14's contract ("classify → retry → accumulate"), so they can be tested in isolation.

What s12 (MCP) and s13 (LSP) will do next: same pattern — add tools to the registry without touching the loop's shape. s14's WithRetry wraps any of them transparently because the retry classifier reads the error type, not the call site.

## Try It

```bash
cd agents/s14-cost-and-recovery

# Demo (deterministic, no network):
go run .

# 5 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 5 tests cover:

1. **TestUsageAddAccumulates** — every one of the five token fields (Input, Output, Reasoning, CacheRead, CacheWrite) accumulates correctly across multiple `Add` calls; the resulting `TotalCost` is positive (catches a regression where Add overwrites instead of summing). Pins the load-bearing math for billing.
2. **TestWithRetryGivesUpAfterMaxAttempts** — when op keeps returning APIError(429), WithRetry calls op exactly `MaxAttempts` times then returns the LAST error (not the first, not nil). Pins both the call count and the returned error type.
3. **TestWithRetryDoesNotRetryAuthError** — AuthError returns immediately; op called exactly once. This is a safety property — retrying auth can trigger account lockout on some providers.
4. **TestShouldCompactOnlyForContextOverflow** — table test over 7 error variants (ContextOverflowError, APIError 429, APIError 500, AuthError, AbortedError, plain stdlib error, nil). Only ContextOverflowError returns true. Pins the contract that compaction never fires for the wrong error.
5. **TestWithRetryRespectsContextCancel** — when ctx fires during a backoff sleep, WithRetry returns `ctx.Err()` in well under the time a full MaxAttempts run would have taken. Pins "ctx beats backoff" so a Ctrl-C feels instant.

## Upstream Source Reading

s14 mirrors two upstream files: `packages/opencode/src/session/session.ts` L91-L142 for the cost / Usage shape, and `packages/opencode/src/session/message-error.ts` L1-L14 for the error taxonomy. The full session.ts is 1000+ lines covering every column on the Session row; we excerpt only the cost-related slice. message-error.ts is 14 lines total — we annotate every line because the whole file IS the taxonomy.

```ts
// upstream:packages/opencode/src/session/session.ts L91-L142

// L91-L98 — the tokens shape inside fromRow. Five integers, one nested
// `cache` sub-object. Our Go Usage flattens the cache (CacheReadTokens
// + CacheWriteTokens) because there's no third cache field to nest with.
return {
  // ... id, slug, projectID, etc. above ...
  cost: row.cost,                                       // ★ Decimal-precision dollars
  tokens: {                                              // ★ five-int record
    input: row.tokens_input,                             //   provider's input rate
    output: row.tokens_output,                           //   output rate
    reasoning: row.tokens_reasoning,                     //   ★ also billed at OUTPUT rate
    cache: {                                             //   nested per Anthropic's API
      read: row.tokens_cache_read,                       //   heavy discount
      write: row.tokens_cache_write,                     //   small premium over input
    },
  },
  // ... share, revert, permission, time, etc. below ...
}

// L112-L142 — toRow: the inverse, persisting Usage to SQLite.
// L131-L135 carries the load-bearing "default to EmptyTokens if null"
// pattern. Our Go side gets this for free: a zero Usage IS the empty
// tokens record — no separate sentinel needed.
export function toRow(info: Info) {
  return {
    // ... id, project_id, etc. above ...
    cost: info.cost ?? 0,                                // ★ default 0 if undefined
    tokens_input: (info.tokens ?? EmptyTokens).input,    // ★ five flat columns
    tokens_output: (info.tokens ?? EmptyTokens).output,  //   on the SQL row
    tokens_reasoning: (info.tokens ?? EmptyTokens).reasoning,
    tokens_cache_read: (info.tokens ?? EmptyTokens).cache.read,
    tokens_cache_write: (info.tokens ?? EmptyTokens).cache.write,
    // ... revert, permission, time_*, etc. below ...
  }
}

// upstream:packages/opencode/src/session/message-error.ts L1-L14

import { Schema } from "effect"
import { NamedError } from "@opencode-ai/core/util/error"

// L4 — OUTPUT-side overflow. Our Go ContextOverflowError covers both
// this and the input-side variant because the recovery is identical:
// COMPACT, then re-issue. ShouldCompact fires on either.
export const OutputLengthError = NamedError.create("MessageOutputLengthError", {})

// L6-L9 — credential failure. Carries providerID so the UI can say
// "your Anthropic key looks bad" instead of just "auth failed."
// This is the canonical "do NOT retry" error.
export const AuthError = NamedError.create("ProviderAuthError", {
  providerID: Schema.String,
  message: Schema.String,
})

// L11-L12 — the union the rest of the codebase dispatches on.
// NamedError.Unknown.EffectSchema is the catch-all (in Go: any error
// not matched by errors.As against our typed sentinels).
export const Shared = [
  AuthError.EffectSchema,
  NamedError.Unknown.EffectSchema,
  OutputLengthError.EffectSchema,
] as const
export const SharedSchema = Schema.Union(Shared)
```

Line-by-line annotation (key lines):

- **session.ts L91-L98 tokens shape** — five integers + nested cache. Our Go Usage struct keeps the five integers (flattened) and uses zero-value semantics for the "no cache hit" case, sparing us a NullableCache wrapper.
- **session.ts L94 reasoning** — billed at OUTPUT rate (Anthropic's policy). Our `TotalCost` folds reasoning into the output term so the math matches the bill; we don't carry a separate `ReasoningPerMTok` that would always equal `OutputPerMTok`.
- **session.ts L130 `info.cost ?? 0`** — the "default to 0" pattern. In Go, an uninitialised `float64` field IS 0; no `?? 0` needed. The same simplification applies to the five token fields.
- **session.ts L131-L135 `?? EmptyTokens`** — the null-safe access pattern. Go's zero-value semantics mean a freshly-allocated `Usage{}` already has all five fields at 0; there's no need for an `EmptyTokens` constant.
- **message-error.ts L4 OutputLengthError** — output-side overflow, distinct from input-side context overflow. Our Go side merges both into `ContextOverflowError` because the recovery (compaction) doesn't care which side overflowed.
- **message-error.ts L6-L9 AuthError** — the load-bearing "don't retry" sentinel. Our Go `*AuthError` is the same shape; `IsRetryable` returns false for it.
- **message-error.ts L11-L12 Schema.Union** — Effect's runtime-checked dispatch surface. Our Go side uses `errors.As` inside the classifiers instead, which is less type-safe at compile time but more idiomatic to read.

Permalinks:

- session.ts L91-L98 (tokens shape in fromRow): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L91-L98>
- session.ts L112-L142 (toRow with cost+tokens): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L112-L142>
- message-error.ts L1-L14 (the whole error taxonomy): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-error.ts#L1-L14>

What we kept, what we cut:

- **Kept** — five-int token shape, reasoning-billed-at-output, the AuthError sentinel, the "context overflow needs compaction" signal, the "give up after MaxAttempts" upper bound.
- **Cut for now** — Decimal.js precision (float64 is fine to ~15 sig digits; swap to shopspring/decimal if you need invoicing accuracy), the Effect-typed Schema.Union dispatch (errors.As in Go is the equivalent), live pricing from the AI SDK provider catalog (we hardcode MOCK constants), the full ai-SDK APIError shape (responseHeaders, requestBodyValues, the `isRetryable` boolean baked into the error itself; we infer from StatusCode), the actual compaction routine (out of scope; ~400 LOC in upstream).
- **Forward-compat** — adding a sixth token field to `Usage` (e.g. `BatchTokens`) is mechanical: extend the struct, the Add method, TotalCost. Adding a new error sentinel (e.g. `*RateLimitError` distinct from APIError 429) is the same: define the type, add a branch to IsRetryable. The classifier-function pattern scales.

opencode cost+recovery reading order:

1. `packages/opencode/src/session/session.ts` L91-L98 — the tokens shape inside `fromRow` (s14's Usage struct mother).
2. `packages/opencode/src/session/session.ts` L112-L142 — `toRow` with cost + tokens columns (s14's persistence target).
3. `packages/opencode/src/session/message-error.ts` L1-L14 — the full error taxonomy (s14's error sentinels).
4. `packages/opencode/src/session/llm.ts` ~L100-L150 — the inlined retry-with-backoff inside streamText (s14's WithRetry factored out).
5. `packages/opencode/src/session/processor.ts` ~L34-L150 — where `ShouldCompact == true` triggers `Session.compact()` (s14's compaction signal consumer; the compaction routine itself is out of scope).
