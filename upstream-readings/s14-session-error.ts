// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/session.ts (Usage shape, L91-L142)
//       packages/opencode/src/session/message-error.ts (error taxonomy, L1-L14)
// permalinks:
//   - https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L91-L142
//   - https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-error.ts#L1-L14
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s14 cares about these two files:
//   They are the cost-accounting + error-recovery seam of opencode. Two
//   regions matter:
//
//     1. session.ts L91-L142 — fromRow + toRow for the cost / tokens
//        columns. The shape that gets persisted on every session row:
//        cost (Decimal-precision dollars) plus a five-int tokens record
//        (input, output, reasoning, cache.read, cache.write). Our Go
//        Usage struct is exactly these five integers, flattened (no
//        nested cache sub-object — Go has no idiomatic reason to nest
//        two ints).
//
//     2. message-error.ts L1-L14 — the error taxonomy. Three NamedError
//        types (OutputLengthError, AuthError, NamedError.Unknown) plus
//        a Schema.Union that the Effect-typed callers downstream
//        dispatch on. Our Go side keeps four error types
//        (APIError, AuthError, ContextOverflowError, AbortedError) and
//        two classifier functions (IsRetryable, ShouldCompact) instead
//        of an isRetryable flag baked into the error itself.
//
// What we rebuilt in Go (s14):
//   - tokens record (5 ints)                    → Usage struct (5 ints)
//   - per-turn `usage.input += event.input`     → Usage.Add
//   - cost as Decimal.js                        → Usage.TotalCost float64
//   - OutputLengthError / ContextOverflowError  → ContextOverflowError
//   - ProviderAuthError                         → AuthError
//   - APIError (defined in @ai-sdk)             → APIError (StatusCode + Body)
//   - upstream's `isRetryable` flag on the err  → IsRetryable(err) classifier
//   - compaction trigger inside the loop        → ShouldCompact(err) classifier
//   - retry-with-backoff (in session/llm.ts)    → WithRetry(ctx, p, op)
//
// What we DID NOT rebuild yet (lives in later sessions or out of scope):
//   - Decimal.js precision for billing — float64 is fine for teaching
//   - the actual compaction routine that runs when ShouldCompact is true
//     (lives in upstream's `session.compact()` ~400 LOC; out of scope)
//   - the full ai-SDK APIError shape (responseHeaders, requestBodyValues,
//     url, isRetryable flag, cause chain) — we keep StatusCode + Body
//   - the Effect-typed Service / Layer / Schema.Union dispatch — Go uses
//     errors.As + a classifier function
//   - reading rates from the AI SDK provider catalog — we hardcode MOCK
//     pricing constants
//
// The 60 lines below are the heart of cost + recovery: the tokens
// record on the way to/from the SQLite row, and the three NamedError
// types that the loop dispatches on.

// =============================================================================
// session.ts L91-L98 — the tokens shape inside fromRow.
// =============================================================================
//
// This is the mother of our Go Usage struct. Five integers, one nested
// cache sub-object. Upstream's `EmptyTokens` (referenced at L131-L135)
// is the all-zeros version we use as the default.
//
// Note `tokens.cache` is nested for symmetry with the Anthropic prompt-
// caching response shape. We flatten in Go (CacheReadTokens,
// CacheWriteTokens) because there's no third cache field — flattening
// reads better than a one-field Cache sub-struct.
return {
  // ... id, slug, projectID, etc. above ...
  cost: row.cost,                                       // ★ Decimal-precision dollars
  tokens: {                                              // ★ five-int record
    input: row.tokens_input,                             //   billed at provider's input rate
    output: row.tokens_output,                           //   billed at output rate
    reasoning: row.tokens_reasoning,                     //   ★ also billed at OUTPUT rate
    cache: {                                             //   nested per Anthropic's API shape
      read: row.tokens_cache_read,                       //   billed at heavy discount
      write: row.tokens_cache_write,                     //   billed at small premium
    },
  },
  // ... share, revert, permission, time, etc. below ...
}

// =============================================================================
// session.ts L112-L142 — toRow: the inverse, persisting Usage to SQLite.
// =============================================================================
//
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

// =============================================================================
// message-error.ts L1-L14 — the error taxonomy.
// =============================================================================
//
// Three NamedError types declared, plus a Schema.Union that downstream
// callers use to dispatch. The pattern is "every error has a tag
// (`_tag`) plus typed payload, and the Schema.Union enforces exhaustive
// handling at the type level."
//
// Our Go side trades the Schema dispatch for `errors.As` type-switching
// inside the IsRetryable / ShouldCompact classifiers. Less type-safe at
// compile time, more idiomatic to read.
import { Schema } from "effect"
import { NamedError } from "@opencode-ai/core/util/error"

// L4 — OUTPUT-side overflow (the model would have produced more tokens
// than its output budget). Our Go ContextOverflowError covers both this
// and the input-side overflow because the recovery is identical:
// COMPACT, then re-issue. ShouldCompact is the predicate that fires
// on either.
export const OutputLengthError = NamedError.create("MessageOutputLengthError", {})

// L6-L9 — credential failure. Carries the providerID so the UI can
// say "your Anthropic key looks bad" instead of just "auth failed."
// Our Go AuthError keeps the Provider field for the same reason.
// This is the canonical "do NOT retry" error: re-sending the same bad
// credential will fail the same way.
export const AuthError = NamedError.create("ProviderAuthError", {
  providerID: Schema.String,
  message: Schema.String,
})

// L11-L12 — the union the rest of the codebase dispatches on.
// NamedError.Unknown.EffectSchema is the "we couldn't classify this"
// catch-all (in Go: any error that's not one of our typed sentinels).
export const Shared = [
  AuthError.EffectSchema,
  NamedError.Unknown.EffectSchema,
  OutputLengthError.EffectSchema,
] as const
export const SharedSchema = Schema.Union(Shared)

// L14 — re-export so upstream's other modules can import this whole
// surface as `MessageError.AuthError`, `MessageError.OutputLengthError`,
// etc. Our Go side just exports each type from the package.
export * as MessageError from "./message-error"

// =============================================================================
// END EXCERPT — what's NOT shown above:
//
// - The actual `isRetryable: boolean` flag attached to APIError lives
//   in `@ai-sdk/anthropic`'s error shape, not opencode itself. Upstream
//   reads it as `err.isRetryable` inside session/llm.ts. Our Go
//   IsRetryable() classifier infers retryability from StatusCode
//   instead, which is enough for 429 + 5xx.
//
// - The retry-with-backoff loop itself lives inside session/llm.ts (around
//   the streamText invocation, ~L100-L150) and isn't a separate function in
//   upstream — it's inlined into the streaming handler. Our Go WithRetry
//   factors it out so the test surface is small.
//
// - The compaction routine (`Session.compact()`, ~400 LOC) lives in a
//   separate file and is out of scope for s14 — we only ship the SIGNAL
//   (ShouldCompact returning true), not the compaction itself.
//
// - Decimal.js cost precision matters for invoicing pipelines but not
//   for teaching. Our float64 TotalCost is good to ~15 significant
//   digits, which is fine for "show the user $0.026700".
// =============================================================================
