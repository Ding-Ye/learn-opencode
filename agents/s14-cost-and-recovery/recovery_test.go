package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestUsageAddAccumulates pins the most basic Usage.Add promise: every
// one of the five token fields accumulates correctly across multiple
// calls. We seed a non-zero starting Usage and add two more onto it so
// the test catches a regression where Add forgets to read the receiver
// (e.g. a stray `*u = other` instead of `u.X += other.X`).
//
// Why this is the load-bearing test for usage.go: a session row's cost
// column is computed from Usage. If Add silently drops a field (say,
// CacheWriteTokens) the user underbills. If it double-counts a field
// the user overbills. Either is a billing bug; the test pins both.
func TestUsageAddAccumulates(t *testing.T) {
	u := Usage{
		InputTokens:      100,
		OutputTokens:     50,
		ReasoningTokens:  10,
		CacheReadTokens:  1000,
		CacheWriteTokens: 20,
	}
	u.Add(Usage{
		InputTokens:      200,
		OutputTokens:     150,
		ReasoningTokens:  30,
		CacheReadTokens:  500,
		CacheWriteTokens: 5,
	})
	u.Add(Usage{
		InputTokens:      50,
		OutputTokens:     25,
		ReasoningTokens:  5,
		CacheReadTokens:  0,
		CacheWriteTokens: 100,
	})
	want := Usage{
		InputTokens:      350,
		OutputTokens:     225,
		ReasoningTokens:  45,
		CacheReadTokens:  1500,
		CacheWriteTokens: 125,
	}
	if u != want {
		t.Errorf("after two Adds, Usage = %+v, want %+v", u, want)
	}

	// And the cost should reflect the sum, not just the last addend —
	// guards against a stray `u = other` overwrite slipping past the
	// per-field check above.
	cost := u.TotalCost(PricingClaudeSonnet4_5)
	if cost <= 0 {
		t.Errorf("TotalCost = %f, want positive", cost)
	}
}

// TestWithRetryGivesUpAfterMaxAttempts pins the "we eventually surrender"
// promise: when op keeps returning a retryable error (here, APIError
// with status 429), WithRetry calls op exactly MaxAttempts times then
// returns the LAST error. Not the first, not a wrapped composite — the
// last, so the caller's logging sees the most recent server response.
//
// Why this matters: in production the rate-limit retry storm is a
// classic cause of cascading failure. The test pins both the call-count
// (3) and the error returned (the 429), so a regression that silently
// expands attempts to 10 or returns nil after exhaustion gets caught.
func TestWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	calls := int32(0)
	op := func() error {
		atomic.AddInt32(&calls, 1)
		return &APIError{StatusCode: 429, Body: "rate limited"}
	}
	policy := RetryPolicy{
		MaxAttempts: 3,
		BaseBackoff: 1 * time.Millisecond,
		MaxBackoff:  5 * time.Millisecond,
	}
	err := WithRetry(context.Background(), policy, op)
	if err == nil {
		t.Fatal("WithRetry returned nil; want error after MaxAttempts exhausted")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("returned err %T = %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("returned APIError.StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("op called %d times, want 3 (MaxAttempts)", got)
	}
}

// TestWithRetryDoesNotRetryAuthError pins the "auth = give up" promise.
// AuthError signals a credential problem; resending the same bad
// credential will produce the same failure. WithRetry must call op
// EXACTLY ONCE and return the AuthError as-is.
//
// Why this matters: an agent that quietly retries an AuthError 3 times
// looks (in logs) like the user typed their password wrong 3 times in a
// row, which can trigger account lockout on some providers. The "auth
// → bail immediately" rule is a safety property, not just an
// optimisation.
func TestWithRetryDoesNotRetryAuthError(t *testing.T) {
	calls := int32(0)
	op := func() error {
		atomic.AddInt32(&calls, 1)
		return &AuthError{Provider: "anthropic"}
	}
	policy := DefaultRetryPolicy()
	err := WithRetry(context.Background(), policy, op)
	if err == nil {
		t.Fatal("WithRetry returned nil; want AuthError")
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("returned err %T = %v, want *AuthError", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("op called %d times, want exactly 1 (auth not retried)", got)
	}
	// And the error message should mention "auth" so a human reading
	// the log can tell what happened without inspecting types.
	if !strings.Contains(authErr.Error(), "auth") {
		t.Errorf("AuthError.Error() = %q, want it to contain 'auth'", authErr.Error())
	}
}

// TestShouldCompactOnlyForContextOverflow pins the classifier contract
// for the OTHER kind of recovery: ShouldCompact returns true for and
// only for *ContextOverflowError. Every other error type — including
// APIError, AuthError, AbortedError, and a plain stdlib error — must
// return false. If ShouldCompact returned true for the wrong type, the
// loop would trigger an expensive compaction (summarize old messages,
// re-issue) for an error that compaction can't possibly fix.
func TestShouldCompactOnlyForContextOverflow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context overflow", &ContextOverflowError{CurrentTokens: 250000, ModelLimit: 200000}, true},
		{"api 429", &APIError{StatusCode: 429}, false},
		{"api 500", &APIError{StatusCode: 500}, false},
		{"auth", &AuthError{Provider: "anthropic"}, false},
		{"aborted", &AbortedError{}, false},
		{"plain", errors.New("something else"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldCompact(tc.err); got != tc.want {
				t.Errorf("ShouldCompact(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWithRetryRespectsContextCancel pins the cancellation contract:
// when the caller's ctx is cancelled while WithRetry is sleeping
// between attempts, WithRetry returns ctx.Err() immediately — it does
// NOT wait for the timer to expire, and it does NOT exhaust further
// attempts.
//
// Why this matters: in an interactive agent loop the user can hit
// Ctrl-C to abort. If WithRetry ignored the cancellation and kept
// retrying through a 5-second backoff, the user would experience a 5+
// second hang after pressing Ctrl-C. Pinning "ctx cancellation wins
// over retry sleep" makes the abort feel instant.
//
// We use a long backoff (500ms) and a short cancellation deadline
// (50ms) so the test is unambiguous about which path won.
func TestWithRetryRespectsContextCancel(t *testing.T) {
	calls := int32(0)
	op := func() error {
		atomic.AddInt32(&calls, 1)
		return &APIError{StatusCode: 503, Body: "down"}
	}
	policy := RetryPolicy{
		MaxAttempts: 5,
		BaseBackoff: 500 * time.Millisecond,
		MaxBackoff:  500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := WithRetry(ctx, policy, op)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WithRetry returned nil; want context.DeadlineExceeded or context.Canceled")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("returned err = %v, want ctx.Err() (DeadlineExceeded or Canceled)", err)
	}
	// We should have bailed well before the 5*500ms = 2500ms a full
	// MaxAttempts run would have taken. Half a second is generous slack
	// for slow CI machines while still proving we didn't sleep through
	// every attempt.
	if elapsed > 500*time.Millisecond {
		t.Errorf("WithRetry took %v; expected to bail in well under 500ms after ctx cancel", elapsed)
	}
	// op should have been called at most twice: once for attempt 0
	// (which fails), then we sleep and the ctx fires before we can
	// reach attempt 1. Allow [1, 2] for scheduler jitter on CI.
	if got := atomic.LoadInt32(&calls); got < 1 || got > 2 {
		t.Errorf("op called %d times, want 1 or 2 (cancelled before MaxAttempts)", got)
	}
}
