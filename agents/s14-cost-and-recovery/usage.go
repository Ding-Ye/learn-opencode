package main

// Usage is the per-request token tally that opencode persists onto every
// session row. Mirrors `packages/opencode/src/session/session.ts` L91-L98:
// upstream stores `tokens.input`, `tokens.output`, `tokens.reasoning`,
// `tokens.cache.read`, `tokens.cache.write` (cache is its own sub-object
// in the TS shape; we flatten to two fields because Go has no idiomatic
// reason to nest two integers).
//
// All five fields matter for billing — Anthropic charges different rates
// for input vs output, reasoning is billed as output, and cache reads
// are billed at a heavy discount (cache writes at a small premium). A
// session that ignores any of the five will under- or over-bill.
//
// Zero values are valid and meaningful: a streaming response with no
// reasoning leaves `ReasoningTokens` at 0; a request that didn't hit the
// cache leaves both cache fields at 0. We do NOT use *int or sql.NullInt
// — Go's zero-value semantics carry the right meaning here.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Add sums `other` into the receiver in place. Used by the session loop
// after every Provider.Stream completes — usage from this turn is
// accumulated onto the running session-wide Usage so the persisted row
// always reflects the WHOLE conversation's cost, not just the last turn.
//
// Mirrors upstream's per-Part `usage.input += event.usage.inputTokens`
// pattern in `session/llm.ts` (the actual TS code is spread across the
// streamText event handlers; the behaviour is "accumulate, never replace").
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

// Pricing is the per-million-tokens rate sheet for a model. We keep the
// four rates the upstream provider catalog tracks: input, output, cache
// read (heavily discounted), and cache write (a small premium over input).
// Reasoning tokens are billed at the OUTPUT rate per Anthropic's policy,
// so we don't carry a separate `ReasoningPerMTok` here — TotalCost folds
// reasoning into output spend.
//
// Units: USD per 1,000,000 tokens. Multiply tokens / 1_000_000 by the
// rate to get dollars.
type Pricing struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// TotalCost returns the dollar cost for `u` at `pricing`. Reasoning
// tokens are billed at the output rate (mirrors Anthropic's billing).
//
// We use float64 instead of upstream's `decimal.js` Decimal — for a
// teaching repo the precision loss is irrelevant (you'll never persist a
// per-session cost beyond two decimals to a UI), and keeping it stdlib
// avoids dragging in shopspring/decimal. If you ever need exact accounting
// (e.g. an invoicing pipeline), swap the four float multiplications for
// `decimal.NewFromFloat(...).Mul(...)` and the call sites stay the same.
func (u Usage) TotalCost(pricing Pricing) float64 {
	const million = 1_000_000.0
	input := float64(u.InputTokens) / million * pricing.InputPerMTok
	output := float64(u.OutputTokens+u.ReasoningTokens) / million * pricing.OutputPerMTok
	cacheRead := float64(u.CacheReadTokens) / million * pricing.CacheReadPerMTok
	cacheWrite := float64(u.CacheWriteTokens) / million * pricing.CacheWritePerMTok
	return input + output + cacheRead + cacheWrite
}

// MOCK pricing constants. Real opencode pulls these from the AI SDK's
// provider model catalog at runtime (see `provider.ts` L500+). For the
// teaching session we hard-code two representative rates so the demo and
// tests have something deterministic to multiply against. Placeholder
// figures only — DO NOT use for real billing decisions.
var (
	// PricingClaudeSonnet4_5 — MOCK. Representative numbers in the
	// ballpark of Anthropic's published Sonnet pricing, but treat as
	// illustrative only. Real rates change; consult the provider's price
	// page at request time.
	PricingClaudeSonnet4_5 = Pricing{
		InputPerMTok:      3.0,
		OutputPerMTok:     15.0,
		CacheReadPerMTok:  0.30,
		CacheWritePerMTok: 3.75,
	}
	// PricingClaudeHaiku4_5 — MOCK. Same caveat — illustrative only.
	PricingClaudeHaiku4_5 = Pricing{
		InputPerMTok:      0.80,
		OutputPerMTok:     4.0,
		CacheReadPerMTok:  0.08,
		CacheWritePerMTok: 1.0,
	}
)
