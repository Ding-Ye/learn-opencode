# s14 — cost-and-recovery

s07 gave the agent a session row to persist. s10 gave it a streaming
tool loop. s14 plugs the missing operational layer between them: **cost
accounting** (so you can put a dollar number on every session) and
**error recovery** (so a 429 doesn't kill the conversation and a context
overflow triggers compaction instead of a stack trace).

The mechanism mirrors `packages/opencode/src/session/session.ts` L91-L142
for the Usage shape and `packages/opencode/src/session/message-error.ts`
for the error taxonomy. ~400 LOC including tests.

## Files

- `usage.go` — the per-request token tally and pricing math:
  - `type Usage struct { InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens int }`
  - `func (u *Usage) Add(other Usage)` — accumulates fields in place.
  - `func (u Usage) TotalCost(pricing Pricing) float64` — reasoning is
    billed at the output rate (mirrors Anthropic's policy).
  - `type Pricing struct { InputPerMTok, OutputPerMTok, CacheReadPerMTok, CacheWritePerMTok float64 }`
  - `PricingClaudeSonnet4_5`, `PricingClaudeHaiku4_5` — MOCK constants
    with placeholder figures. DO NOT use for real billing.
- `errors.go` — the four error types and two classifiers:
  - `*APIError{StatusCode, Body}` — transport / server failures.
  - `*AuthError{Provider}` — credential failures (no retry).
  - `*ContextOverflowError{CurrentTokens, ModelLimit}` — too-long
    payload (compact instead of retry).
  - `*AbortedError{}` — caller cancelled (don't retry, don't compact).
  - `IsRetryable(err) bool` — true for APIError 429 or 5xx, false for
    everything else (including non-typed errors).
  - `ShouldCompact(err) bool` — true ONLY for ContextOverflowError.
- `retry.go` — the policy + the wrapper:
  - `type RetryPolicy struct { MaxAttempts int; BaseBackoff time.Duration; MaxBackoff time.Duration }`
  - `DefaultRetryPolicy()` → 3 attempts, 200ms base, 5s max.
  - `WithRetry(ctx, p, op)` — runs op; on retryable err, sleeps with
    exponential backoff (capped at MaxBackoff); respects ctx.Done();
    returns the LAST error if all attempts failed.
- `main.go` — short demo. Builds a Usage, accumulates two requests,
  prints the total + cost. Then wraps a flaky function (fails twice
  with 503, succeeds on attempt 3) in WithRetry. Deterministic, no
  network.
- `recovery_test.go` — 5 tests:
  1. **TestUsageAddAccumulates** — every field on Usage accumulates
     correctly across multiple Add calls; cost reflects the sum.
  2. **TestWithRetryGivesUpAfterMaxAttempts** — retries on APIError(429)
     up to MaxAttempts times then returns the LAST error.
  3. **TestWithRetryDoesNotRetryAuthError** — AuthError returns
     immediately; op called exactly once.
  4. **TestShouldCompactOnlyForContextOverflow** — table test covering
     7 error variants; only ContextOverflowError returns true.
  5. **TestWithRetryRespectsContextCancel** — when ctx fires during
     a backoff sleep, returns ctx.Err() in well under MaxAttempts time.

## Run

```bash
# Demo (deterministic, no network)
go run .

# 5 tests
go test -count=1 ./...

# Vet + build + test in one go
go vet ./... && go build ./... && go test -count=1 ./...
```

## Key teaching points

- **Two recoveries, two predicates.** `IsRetryable` and `ShouldCompact`
  are deliberately separate functions, not branches of a single
  classifier. Retry sends the SAME payload; compaction MUTATES the
  payload (drops or summarizes messages). Conflating them invites
  bugs where a context-overflow error gets retried 3 times before
  someone notices the payload is too big.
- **Last error wins.** `WithRetry` returns the LAST error, not the
  first or a wrapped composite. The caller's logging sees the most
  recent server response, which is usually the most informative
  (a transient 503 followed by a permanent 503 should surface the
  permanent one).
- **Ctx beats backoff.** Cancellation always wins over a sleeping
  retry. An interactive user who hits Ctrl-C should not have to wait
  out a 5-second backoff. The select-on-ctx.Done() pattern in
  `WithRetry` is the load-bearing piece.
- **Reasoning bills like output.** Anthropic charges reasoning tokens
  at the output rate. `TotalCost` folds them into the output term so
  the math matches the bill.
- **MOCK pricing.** The `PricingClaude*` constants are illustrative.
  Real opencode pulls live rates from the AI SDK's provider catalog.
  For a teaching repo, hardcoded numbers keep the demo deterministic.

See `docs/zh/s14-cost-and-recovery.md` and `docs/en/s14-cost-and-recovery.md`
for the long-form walkthrough plus the upstream `session.ts` cost
section + `message-error.ts` error types annotated.
