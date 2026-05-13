package main

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Loop consumes a Provider.Stream and assembles its Events into one Message
// of Parts. It is the streaming-aware upgrade of s05's "wait for full
// response then process" pattern — instead of blocking until the LLM
// finishes, it pulls Events one at a time and appends them to an
// in-construction Message, so a future caller (s10's tool loop) can react
// mid-stream without waiting for `message_stop`.
//
// The Loop is *only* the assembler. It does NOT:
//   - dispatch tool calls (s10's job — Loop just records them as Parts)
//   - run permission checks (s04 / s10)
//   - persist anything (s07's session store)
//   - retry on errors (s14)
//
// Keeping Loop deliberately narrow is what lets each later session add one
// concern without rewriting the streaming layer. Compare to opencode's
// `packages/opencode/src/session/processor.ts` which fuses streaming +
// dispatch + persistence into one `Handle.process()` call — readable code,
// but harder to teach incrementally.
type Loop struct {
	Provider Provider
}

// Consume opens Provider.Stream(ctx, req), then in a for-loop calls Next()
// until io.EOF, accumulating each Event into a *Message. Returns the
// assembled Message and a nil error on clean end; returns context.Canceled
// (or context.DeadlineExceeded) if the caller cancels mid-stream; returns a
// wrapped error for any malformed Event (e.g. a tool_use without a name).
//
// Assembly rules:
//
//   - EventText:      adjacent text deltas collapse into ONE PartText. A
//                     tool_use or reasoning block in between starts a fresh
//                     PartText for the next text.
//   - EventToolUse:   appends ONE PartToolUse with the (already buffered)
//                     input. Provider impls (s05's anthropicStream) must
//                     emit this only once per tool_use block.
//   - EventReasoning: adjacent reasoning chunks collapse into ONE
//                     PartReasoning. Same boundary rule as text.
//   - EventFinish:    records Usage on the Message; the next Next() should
//                     return io.EOF.
//
// On context cancellation, Consume calls stream.Close() and returns
// ctx.Err() — the load-bearing contract s10 relies on for "user pressed
// Ctrl-C, abort everything in flight."
func (l *Loop) Consume(ctx context.Context, req Request) (*Message, error) {
	if l == nil || l.Provider == nil {
		return nil, errors.New("loop: nil Provider")
	}

	stream, err := l.Provider.Stream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provider.Stream: %w", err)
	}
	defer stream.Close()

	msg := &Message{Role: RoleAssistant}

	// trailing tracks the kind of the most recently appended Part so we know
	// whether the *next* text/reasoning chunk should extend it (same kind in
	// a row) or start a new Part (a tool_use happened in between, breaking
	// the run). Zero value PartUnknown means "nothing appended yet."
	trailing := PartUnknown

	for {
		// Cancellation check before every Next() — guarantees that even if
		// the Provider impl is slow to honor ctx itself, the Loop bails on
		// the next iteration. This mirrors what `for ev := range channel`
		// loops in idiomatic Go would do via a `select` against ctx.Done().
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return msg, nil
		}
		if err != nil {
			// If the underlying error is a context error (the Provider
			// impl honored cancellation by surfacing it through Next),
			// preserve that so the caller sees context.Canceled, not a
			// wrapped "stream error: ..." message.
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, fmt.Errorf("stream.Next: %w", err)
		}

		switch ev.Type {
		case EventText:
			if trailing == PartText && len(msg.Parts) > 0 {
				// Extend the current run of text by appending to the last
				// PartText.Text. This is the streaming version of "the
				// model is mid-sentence" — N deltas, one prose block.
				last := &msg.Parts[len(msg.Parts)-1]
				if last.Text == nil {
					last.Text = &TextPart{}
				}
				last.Text.Text += ev.Text
			} else {
				msg.Parts = append(msg.Parts, Part{
					Kind: PartText,
					Text: &TextPart{Text: ev.Text},
				})
			}
			trailing = PartText

		case EventToolUse:
			if ev.ToolUse == nil {
				return nil, errors.New("loop: EventToolUse with nil ToolUse")
			}
			if ev.ToolUse.Name == "" {
				// A tool_use without a name is unusable — the Loop can't
				// dispatch it and a downstream consumer (s10) would get
				// a cryptic "unknown tool ''" error. Fail loud here so
				// the bug points at the Provider impl, not the consumer.
				return nil, fmt.Errorf("loop: EventToolUse missing tool name (id=%q)", ev.ToolUse.ID)
			}
			msg.Parts = append(msg.Parts, Part{
				Kind: PartToolUse,
				ToolUse: &ToolUsePart{
					ID:    ev.ToolUse.ID,
					Name:  ev.ToolUse.Name,
					Input: ev.ToolUse.Input,
				},
			})
			trailing = PartToolUse

		case EventReasoning:
			if trailing == PartReasoning && len(msg.Parts) > 0 {
				last := &msg.Parts[len(msg.Parts)-1]
				if last.Reasoning == nil {
					last.Reasoning = &ReasoningPart{}
				}
				last.Reasoning.Text += ev.Reasoning
			} else {
				msg.Parts = append(msg.Parts, Part{
					Kind:      PartReasoning,
					Reasoning: &ReasoningPart{Text: ev.Reasoning},
				})
			}
			trailing = PartReasoning

		case EventFinish:
			// Record Usage on the message. We don't break here — the
			// Provider contract is "EventFinish, then io.EOF on the next
			// Next()" (see s05 README). The next iteration will see EOF
			// and return cleanly.
			if ev.Usage != nil {
				usageCopy := *ev.Usage
				msg.Usage = &usageCopy
			}
			// stop_reason is what opencode tracks as "did the model finish
			// or did it ask for a tool?". s05's Event doesn't carry it
			// separately; we infer "tool_use" from the presence of a
			// PartToolUse and "end_turn" otherwise. s10 will revisit this
			// when it needs the explicit reason for the loop-vs-stop choice.
			if msg.StopReason == "" {
				msg.StopReason = inferStopReason(msg.Parts)
			}

		default:
			// Unknown event type — opencode logs and ignores. We do the
			// same: a future Anthropic addition shouldn't break the Loop.
		}
	}
}

// inferStopReason picks "tool_use" if the assembled message ended with a
// tool_use Part (so s10's loop knows to dispatch and re-ask), "end_turn"
// otherwise. opencode's wire layer carries this explicitly in
// `message_delta.delta.stop_reason`; s05's Event union doesn't surface that
// field yet, so we infer. Cheap and close-enough until s10 adds the
// explicit field.
func inferStopReason(parts []Part) string {
	if len(parts) == 0 {
		return "end_turn"
	}
	if parts[len(parts)-1].Kind == PartToolUse {
		return "tool_use"
	}
	return "end_turn"
}
