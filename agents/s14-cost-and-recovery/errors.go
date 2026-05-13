package main

import (
	"errors"
	"fmt"
)

// APIError is a transport- or server-level failure from the upstream
// provider. StatusCode is the HTTP status (or 0 if the failure happened
// before the server replied — DNS, connection reset, etc.); Body is the
// raw response body if available, useful for surfacing the provider's
// own error message in logs and tests.
//
// Mirrors upstream's `APIError` in `packages/opencode/src/session/
// message-error.ts` — same idea: any HTTP-shaped failure that isn't
// auth or context-overflow lands here. The classifier (IsRetryable)
// reads StatusCode to decide whether retry is worth trying.
type APIError struct {
	StatusCode int
	Body       string
}

// Error implements the error interface. Format is stable enough to
// substring-match in tests: "API error: status=503 body=..."
func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("API error: status=%d", e.StatusCode)
	}
	return fmt.Sprintf("API error: status=%d body=%s", e.StatusCode, e.Body)
}

// AuthError signals a credential failure — bad API key, expired OAuth
// token, missing entitlement. Distinct from APIError because retry is
// pointless: re-sending the same bad credential will fail the same way.
// The upstream loop surfaces this to the user as "re-authenticate" UI.
//
// Mirrors `ProviderAuthError` in upstream's message-error.ts (the file
// only declares it as a NamedError with `providerID + message` fields;
// we keep just `Provider` because the Go side surfaces detail through
// error wrapping rather than an extra field).
type AuthError struct {
	Provider string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("auth error: provider=%s (re-authenticate required)", e.Provider)
}

// ContextOverflowError signals the request exceeded the model's context
// window — usually because the conversation got long and the system
// prompt + history + new user message no longer fits. The recovery is
// NOT retry: the same too-big payload will fail the same way. The
// recovery is **compaction** — summarize older messages, drop redundant
// parts, re-issue the request. ShouldCompact returns true for this and
// only this error type.
//
// CurrentTokens / ModelLimit let the compaction routine make a smart
// decision about how aggressively to summarize: if you're at 200k of a
// 200k window, you need to drop a LOT; if you're at 205k of 200k, you
// can probably trim just one big tool result.
//
// Upstream calls this `OutputLengthError` for output-side overflow and
// `ContextOverflowError` (in newer code paths) for input-side. We use a
// single name covering both because the recovery is the same.
type ContextOverflowError struct {
	CurrentTokens int
	ModelLimit    int
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context overflow: %d tokens > %d limit (compact required)", e.CurrentTokens, e.ModelLimit)
}

// AbortedError is the sentinel for "the user (or a parent ctx) cancelled
// this request". It is NOT a failure — the loop should propagate the
// cancellation up, NOT retry, NOT compact. Mirrors upstream's
// `AbortedError` (a named error returned from streamText when its
// abortSignal fires).
//
// Carries no fields because there's nothing useful to say beyond "this
// got cancelled" — the cancellation reason lives on the ctx the caller
// passed in.
type AbortedError struct{}

func (e *AbortedError) Error() string { return "request aborted" }

// IsRetryable returns true iff `err` represents a transient transport
// failure that has a reasonable chance of succeeding on retry. The two
// cases:
//
//  1. `*APIError` with StatusCode 429 (rate limited) — server explicitly
//     asks us to back off and try again later.
//  2. `*APIError` with StatusCode >= 500 (server-side error) — gateway,
//     timeout, internal error; usually transient.
//
// Everything else is NON-retryable:
//
//   - `*AuthError` — bad credential won't fix itself
//   - `*ContextOverflowError` — same payload will fail the same way; the
//     fix is compaction, not retry
//   - `*AbortedError` — caller asked to stop; don't fight them
//   - `*APIError` with 4xx other than 429 — a 400 means our request body
//     is malformed; a 404 means the model ID doesn't exist; retrying
//     won't fix either
//   - any non-typed error — if the caller didn't bother to wrap, we
//     assume they didn't intend it to be retryable
//
// Mirrors the `isRetryable` flag pattern that upstream's message-error.ts
// types carry. Upstream attaches the flag to the error itself; Go uses a
// classifier function instead because errors.As makes type-switching
// idiomatic.
func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return true
		}
		if apiErr.StatusCode >= 500 {
			return true
		}
	}
	return false
}

// ShouldCompact returns true iff `err` is the specific signal "the
// conversation is too long for the model's context window — summarize
// and try again." This is exactly `*ContextOverflowError`; nothing else
// matches. The caller's job (in s10's loop) is to catch this from
// WithRetry's return value and trigger compaction before re-issuing.
//
// We could overload IsRetryable to fold this in, but compaction is a
// fundamentally different recovery: it MUTATES the request payload (drops
// or summarizes messages). Retry-with-backoff sends the SAME payload.
// Two recoveries, two predicates.
func ShouldCompact(err error) bool {
	var ctxErr *ContextOverflowError
	return errors.As(err, &ctxErr)
}
