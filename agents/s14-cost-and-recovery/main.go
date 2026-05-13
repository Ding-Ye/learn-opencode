package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// main is a hand-runnable demo of the cost + recovery mechanisms:
//
//	go run .
//
// It does two things, in order:
//
//  1. Build a Usage, accumulate two requests' worth of token counts
//     into it, and print the running total + the dollar cost at our
//     mock claude-sonnet-4-5 pricing.
//  2. Wrap a deliberately-flaky function in WithRetry. The function
//     fails twice with APIError(503) and succeeds on the third call,
//     proving the retry-with-backoff classifier works end-to-end.
//
// Deterministic, no network, no env vars touched. Demo prints exactly
// what s10's loop would log between turns.
func main() {
	// Part 1 — Usage accumulation + cost.
	var sessionUsage Usage
	turn1 := Usage{
		InputTokens:      1200,
		OutputTokens:     350,
		ReasoningTokens:  80,
		CacheReadTokens:  10000,
		CacheWriteTokens: 200,
	}
	turn2 := Usage{
		InputTokens:      900,
		OutputTokens:     420,
		ReasoningTokens:  60,
		CacheReadTokens:  10000,
		CacheWriteTokens: 0,
	}
	sessionUsage.Add(turn1)
	sessionUsage.Add(turn2)

	fmt.Fprintln(os.Stdout, "=== Usage after 2 turns ===")
	fmt.Fprintf(os.Stdout, "  input        = %d tokens\n", sessionUsage.InputTokens)
	fmt.Fprintf(os.Stdout, "  output       = %d tokens\n", sessionUsage.OutputTokens)
	fmt.Fprintf(os.Stdout, "  reasoning    = %d tokens\n", sessionUsage.ReasoningTokens)
	fmt.Fprintf(os.Stdout, "  cache.read   = %d tokens\n", sessionUsage.CacheReadTokens)
	fmt.Fprintf(os.Stdout, "  cache.write  = %d tokens\n", sessionUsage.CacheWriteTokens)
	fmt.Fprintf(os.Stdout, "  cost (sonnet, MOCK) = $%.6f\n", sessionUsage.TotalCost(PricingClaudeSonnet4_5))
	fmt.Fprintf(os.Stdout, "  cost (haiku,  MOCK) = $%.6f\n", sessionUsage.TotalCost(PricingClaudeHaiku4_5))

	// Part 2 — WithRetry around a flaky operation.
	fmt.Fprintln(os.Stdout, "\n=== WithRetry demo (flaky func, 503 x2 then OK) ===")
	calls := 0
	flaky := func() error {
		calls++
		if calls < 3 {
			fmt.Fprintf(os.Stdout, "  attempt %d → APIError(503)\n", calls)
			return &APIError{StatusCode: 503, Body: "service unavailable"}
		}
		fmt.Fprintf(os.Stdout, "  attempt %d → ok\n", calls)
		return nil
	}

	// Use a short-backoff policy so the demo doesn't sit idle for seconds.
	policy := RetryPolicy{
		MaxAttempts: 5,
		BaseBackoff: 50 * time.Millisecond,
		MaxBackoff:  200 * time.Millisecond,
	}
	if err := WithRetry(context.Background(), policy, flaky); err != nil {
		fmt.Fprintln(os.Stderr, "WithRetry failed:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "  succeeded after %d call(s)\n", calls)
	fmt.Fprintln(os.Stdout, "\n(s10's loop would wrap each Provider.Stream call in WithRetry like this.)")
}
