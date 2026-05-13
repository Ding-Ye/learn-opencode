package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLoopTextOnlyStream pins the simplest assembly rule: N adjacent
// EventText deltas collapse into ONE PartText. This is the streaming
// version of s01's "parse one text response" — same outcome ("Hello world"),
// different shape arriving over the wire (5 deltas instead of one blob).
//
// If a future Loop refactor accidentally appended one PartText per delta,
// this test fires. Catching that early matters because s07 will persist
// the Parts to SQLite — fan-out would be N rows per assistant turn, not 1.
func TestLoopTextOnlyStream(t *testing.T) {
	provider := &fakeProvider{
		events: []Event{
			{Type: EventText, Text: "Hello"},
			{Type: EventText, Text: " "},
			{Type: EventText, Text: "world"},
			{Type: EventFinish, Usage: &Usage{InputTokens: 4, OutputTokens: 2}},
		},
	}

	loop := &Loop{Provider: provider}
	msg, err := loop.Consume(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	if msg.Role != RoleAssistant {
		t.Errorf("Role = %q, want %q", msg.Role, RoleAssistant)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("got %d Parts, want 1: %+v", len(msg.Parts), msg.Parts)
	}
	if msg.Parts[0].Kind != PartText {
		t.Fatalf("Parts[0].Kind = %v, want PartText", msg.Parts[0].Kind)
	}
	if msg.Parts[0].Text == nil || msg.Parts[0].Text.Text != "Hello world" {
		t.Errorf("Parts[0].Text = %+v, want 'Hello world'", msg.Parts[0].Text)
	}
	if msg.Usage == nil || msg.Usage.InputTokens != 4 || msg.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want input=4 output=2", msg.Usage)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", msg.StopReason)
	}
}

// TestLoopInterleavedToolUseAndText pins the boundary rule: a tool_use
// breaks an in-progress text run, so the next text after it starts a fresh
// PartText. The result is text → tool_use → text — three Parts in arrival
// order, NOT a single concatenated text plus a tool_use somewhere.
//
// This is the load-bearing test for s10's tool loop: the "should I dispatch
// a tool?" decision walks msg.Parts in order, and conflating the text on
// either side of a tool_use would change the meaning of the message
// (tool result feedback would land in the wrong context).
func TestLoopInterleavedToolUseAndText(t *testing.T) {
	provider := &fakeProvider{
		events: []Event{
			{Type: EventText, Text: "I'll call edit: "},
			{Type: EventToolUse, ToolUse: &ToolUseEvent{
				ID:    "toolu_01ABC",
				Name:  "edit",
				Input: json.RawMessage(`{"path":"a.go"}`),
			}},
			{Type: EventText, Text: "done."},
			{Type: EventFinish, Usage: &Usage{InputTokens: 9, OutputTokens: 5}},
		},
	}

	loop := &Loop{Provider: provider}
	msg, err := loop.Consume(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	if len(msg.Parts) != 3 {
		t.Fatalf("got %d Parts, want 3: %+v", len(msg.Parts), msg.Parts)
	}

	// Part 0: PartText "I'll call edit: "
	if msg.Parts[0].Kind != PartText {
		t.Fatalf("Parts[0].Kind = %v, want PartText", msg.Parts[0].Kind)
	}
	if msg.Parts[0].Text == nil || msg.Parts[0].Text.Text != "I'll call edit: " {
		t.Errorf("Parts[0].Text = %+v, want 'I'll call edit: '", msg.Parts[0].Text)
	}

	// Part 1: PartToolUse with full input
	if msg.Parts[1].Kind != PartToolUse {
		t.Fatalf("Parts[1].Kind = %v, want PartToolUse", msg.Parts[1].Kind)
	}
	tu := msg.Parts[1].ToolUse
	if tu == nil {
		t.Fatal("Parts[1].ToolUse is nil")
	}
	if tu.ID != "toolu_01ABC" || tu.Name != "edit" {
		t.Errorf("ToolUse id/name = %q/%q, want toolu_01ABC/edit", tu.ID, tu.Name)
	}
	if string(tu.Input) != `{"path":"a.go"}` {
		t.Errorf("ToolUse.Input = %q, want %q", string(tu.Input), `{"path":"a.go"}`)
	}

	// Part 2: PartText "done." — a FRESH PartText, not concatenated onto Part 0.
	if msg.Parts[2].Kind != PartText {
		t.Fatalf("Parts[2].Kind = %v, want PartText", msg.Parts[2].Kind)
	}
	if msg.Parts[2].Text == nil || msg.Parts[2].Text.Text != "done." {
		t.Errorf("Parts[2].Text = %+v, want 'done.'", msg.Parts[2].Text)
	}

	// stop_reason inferred as end_turn because the LAST Part is text, not tool_use.
	// (If the model had ended on a tool_use the loop would re-ask; this case ends.)
	if msg.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", msg.StopReason)
	}
}

// TestLoopAbortsOnContextCancel pins the abort contract: if ctx is canceled
// mid-stream, Consume returns context.Canceled (or DeadlineExceeded), NOT a
// partial Message. s10's user-facing Ctrl-C handling will rely on this —
// without it, an interrupted request would silently complete-and-discard.
//
// We use the fakeStream's blockOn channel to guarantee we're actually
// mid-stream when Cancel() fires (otherwise the test could race the
// emit-all-events-and-EOF path and become flaky).
func TestLoopAbortsOnContextCancel(t *testing.T) {
	block := make(chan struct{})
	defer close(block) // cleanup if the test exits without unblocking

	provider := &fakeProvider{
		events: []Event{
			{Type: EventText, Text: "starting..."},
			// After this 1st event, Stream.Next() blocks until cancel
			// fires. Without cancel, the test would hang — that's
			// intentional; the test must prove cancel is what unblocks.
			{Type: EventText, Text: "should never see this"},
			{Type: EventFinish, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
		blockOn:      block,
		unblockAfter: 1, // 1 event emitted, then block
	}

	ctx, cancel := context.WithCancel(context.Background())
	loop := &Loop{Provider: provider}

	// Cancel after a tiny delay so the Loop has time to consume the first
	// event and reach the blocking Next() call. 50ms is well under any
	// reasonable test timeout but well over the loop's per-iteration cost.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	msg, err := loop.Consume(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if msg != nil {
		t.Errorf("msg = %+v, want nil on abort", msg)
	}
}

// TestLoopRejectsMalformedToolUse pins the validation contract: if a
// Provider impl emits an EventToolUse without a name (or with a nil
// ToolUse pointer), the Loop fails fast with a clear error. The
// alternative — silently appending a PartToolUse with an empty Name —
// would push the bug to s10's tool dispatcher, where the failure ("unknown
// tool ''") is much harder to trace back to its origin.
//
// We also pin that the error path doesn't leak a partial Message: the
// caller gets (nil, err), not (half-built msg, err) — which would tempt
// a careless caller to use the Message anyway.
func TestLoopRejectsMalformedToolUse(t *testing.T) {
	provider := &fakeProvider{
		events: []Event{
			{Type: EventText, Text: "I'll call: "},
			{Type: EventToolUse, ToolUse: &ToolUseEvent{
				ID:    "toolu_bad",
				Name:  "", // ← the bug
				Input: json.RawMessage(`{}`),
			}},
		},
	}

	loop := &Loop{Provider: provider}
	msg, err := loop.Consume(context.Background(), Request{})

	if err == nil {
		t.Fatal("err = nil, want error for missing tool name")
	}
	if !strings.Contains(err.Error(), "tool name") {
		t.Errorf("err = %v, want error mentioning 'tool name'", err)
	}
	if msg != nil {
		t.Errorf("msg = %+v, want nil on malformed event", msg)
	}
}
