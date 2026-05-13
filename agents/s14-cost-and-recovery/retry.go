package main

import (
	"context"
	"time"
)

// RetryPolicy is a tiny three-field config that tells WithRetry how
// hard to try and how long to wait between attempts. Defaults are
// chosen for an interactive agent loop:
//
//   - MaxAttempts = 3 — first attempt + 2 retries. Beyond 3 the user is
//     waiting too long for an autonomous loop to recover; surface the
//     error to them so they can decide.
//   - BaseBackoff = 200ms — short enough that a 429 from a healthy
//     provider rarely interrupts the conversation, long enough to avoid
//     hammering on an actually-overloaded server.
//   - MaxBackoff = 5s — even on attempt 10 (if MaxAttempts allowed it)
//     we cap the sleep at 5s. An interactive user shouldn't see a
//     longer pause without an explicit "still working…" message.
//
// Backoff schedule with these defaults: 200ms, 400ms (capped at 5s well
// before saturating), then we're out of attempts. Exponential growth
// keeps the worst-case tolerable while protecting against thundering-
// herd retry storms.
type RetryPolicy struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultRetryPolicy returns the policy described above. Callers that
// want different behaviour (a CI test that wants 1 attempt, an offline
// batch that wants 10 attempts and 30s max) instantiate RetryPolicy
// directly — there's no "config from file" plumbing because retry policy
// shouldn't change between sessions of the same agent.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseBackoff: 200 * time.Millisecond,
		MaxBackoff:  5 * time.Second,
	}
}

// WithRetry runs `op` up to `p.MaxAttempts` times, sleeping between
// attempts with exponential backoff (capped at p.MaxBackoff). Three
// load-bearing rules:
//
//  1. **Classify before retrying.** Only `IsRetryable(err)` errors
//     trigger another attempt. AuthError, ContextOverflowError, and
//     AbortedError return immediately on the first hit — re-running op
//     would burn an attempt (and possibly money) for nothing.
//
//  2. **Respect ctx cancellation.** If `ctx.Done()` fires while we're
//     sleeping between attempts, we return `ctx.Err()` immediately
//     without consuming further attempts. Mirrors upstream's check on
//     the abort signal at every retry boundary.
//
//  3. **Return the LAST error if all attempts fail.** Not the first,
//     not a wrapped composite — the last, so the caller's logging and
//     fallback logic sees the most recent server response. This matches
//     upstream's `lastError` accumulator pattern.
//
// `op` is a func that may be called multiple times. If it has side
// effects beyond "send HTTP request", the caller is responsible for
// making it idempotent or guarding against double-execution.
func WithRetry(ctx context.Context, p RetryPolicy, op func() error) error {
	var lastErr error
	backoff := p.BaseBackoff
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		// Check ctx FIRST — if the caller cancelled while we were
		// sleeping (or before the first attempt), don't waste a
		// network round-trip on a request whose result will be
		// discarded.
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsRetryable(err) {
			// Non-retryable: AuthError, ContextOverflowError,
			// AbortedError, or any 4xx that isn't 429. Return it
			// immediately so the outer loop can dispatch on type
			// (compaction for ContextOverflowError, re-auth for
			// AuthError, etc).
			return err
		}
		// We're going to retry. Don't sleep on the LAST attempt — the
		// retry loop is about to exit anyway, and sleeping just delays
		// the user's error message.
		if attempt == p.MaxAttempts-1 {
			break
		}
		// Sleep with cancellation: if ctx fires during the sleep, bail
		// out with ctx.Err(). select-on-channel + timer is the
		// idiomatic Go way to make a sleep cancellable.
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		// Exponential backoff with cap. Doubling is the common default;
		// jitter would be nice for thundering-herd avoidance but isn't
		// required at MaxAttempts=3.
		backoff *= 2
		if backoff > p.MaxBackoff {
			backoff = p.MaxBackoff
		}
	}
	return lastErr
}
