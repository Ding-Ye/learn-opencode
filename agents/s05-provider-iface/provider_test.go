package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseStubServer returns an httptest.Server that, on any POST, replies with
// `body` as text/event-stream. We assert request shape (api key, version,
// stream flag) inline so the test failure points at the wrong field.
func sseStubServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong anthropic-version: %q", r.Header.Get("anthropic-version"))
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("missing Accept: text/event-stream, got %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
}

// drain pulls Events from a Stream until io.EOF, returning the slice. Any
// non-EOF error is reported via t.Fatalf so callers get a clean stack trace.
func drain(t *testing.T, s Stream) []Event {
	t.Helper()
	var out []Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stream.Next: %v", err)
		}
		out = append(out, ev)
	}
}

// TestStreamTextEvents exercises the simplest happy path: a stream of
// content_block_delta events of type text_delta should produce a sequence of
// EventText events whose concatenation is the assistant's text.
//
// This is the s05 analogue of s01's "parse one text response" test — but in
// the streaming world the same prose arrives as N deltas instead of one blob.
func TestStreamTextEvents(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":7,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv := sseStubServer(t, body)
	defer srv.Close()

	p := NewAnthropicProvider("k", "claude-test").withBaseURL(srv.URL)
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	events := drain(t, stream)

	// Expect: 2 EventText (Hello, " world") + 1 EventFinish.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Type != EventText || events[0].Text != "Hello" {
		t.Errorf("event[0] = %+v, want EventText 'Hello'", events[0])
	}
	if events[1].Type != EventText || events[1].Text != " world" {
		t.Errorf("event[1] = %+v, want EventText ' world'", events[1])
	}
	if events[2].Type != EventFinish {
		t.Errorf("event[2].Type = %v, want EventFinish", events[2].Type)
	}
}

// TestStreamToolUseEvent exercises the tool_use path: a content_block_start
// of type tool_use, followed by N input_json_delta deltas, then a
// content_block_stop should produce ONE EventToolUse with the buffered JSON
// parsed as input. This is the load-bearing assertion that makes s10's
// "dispatch tool when LLM calls one" possible.
func TestStreamToolUseEvent(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_2","role":"assistant","usage":{"input_tokens":12,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"edit","input":{}}}`,
		``,
		// Stream the input JSON in three pieces to prove buffering works.
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\","}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"old\":\"x\",\"new\":\"y\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":17}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv := sseStubServer(t, body)
	defer srv.Close()

	p := NewAnthropicProvider("k", "claude-test").withBaseURL(srv.URL)
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "edit a.go"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	events := drain(t, stream)

	// Expect: 1 EventToolUse + 1 EventFinish. The 3 input_json_delta SSE
	// events should NOT each produce an Event — they're buffered.
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Type != EventToolUse {
		t.Fatalf("event[0].Type = %v, want EventToolUse", events[0].Type)
	}
	tu := events[0].ToolUse
	if tu == nil {
		t.Fatal("event[0].ToolUse is nil")
	}
	if tu.ID != "toolu_01ABC" {
		t.Errorf("ToolUse.ID = %q, want toolu_01ABC", tu.ID)
	}
	if tu.Name != "edit" {
		t.Errorf("ToolUse.Name = %q, want edit", tu.Name)
	}
	wantInput := `{"path":"a.go","old":"x","new":"y"}`
	if string(tu.Input) != wantInput {
		t.Errorf("ToolUse.Input = %q, want %q", string(tu.Input), wantInput)
	}
	if events[1].Type != EventFinish {
		t.Errorf("event[1].Type = %v, want EventFinish", events[1].Type)
	}
}

// TestStreamReasoningEvent exercises Anthropic's extended-thinking shape:
// a content_block of type "thinking" emits thinking_delta events that we
// surface as EventReasoning. s14 will use these to bill reasoning tokens
// separately; s10 must NOT feed them back to the model as user input.
func TestStreamReasoningEvent(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_3","role":"assistant","usage":{"input_tokens":4,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv := sseStubServer(t, body)
	defer srv.Close()

	p := NewAnthropicProvider("k", "claude-test").withBaseURL(srv.URL)
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "think"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	events := drain(t, stream)

	// Expect: 1 EventReasoning + 1 EventFinish.
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Type != EventReasoning {
		t.Fatalf("event[0].Type = %v, want EventReasoning", events[0].Type)
	}
	if events[0].Reasoning != "Let me think..." {
		t.Errorf("event[0].Reasoning = %q, want 'Let me think...'", events[0].Reasoning)
	}
	if events[1].Type != EventFinish {
		t.Errorf("event[1].Type = %v, want EventFinish", events[1].Type)
	}
}

// TestStreamFinishUsageAndEOF pins two contracts together:
//
//  1. message_stop produces an EventFinish whose Usage carries the input
//     tokens from message_start AND the output tokens from message_delta
//     (the protocol splits them across two events, we re-join them).
//  2. The Next() call AFTER EventFinish returns (Event{}, io.EOF) — that's
//     the Go-idiomatic stream-done signal s06's loop will rely on for its
//     `for { ev, err := stream.Next(); if errors.Is(err, io.EOF) { break } }`.
func TestStreamFinishUsageAndEOF(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_4","role":"assistant","usage":{"input_tokens":42,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":99}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv := sseStubServer(t, body)
	defer srv.Close()

	p := NewAnthropicProvider("k", "claude-test").withBaseURL(srv.URL)
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	// First call: EventText "ok".
	ev, err := stream.Next()
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	if ev.Type != EventText || ev.Text != "ok" {
		t.Errorf("event[0] = %+v, want EventText 'ok'", ev)
	}

	// Second call: EventFinish with merged usage.
	ev, err = stream.Next()
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if ev.Type != EventFinish {
		t.Fatalf("event[1].Type = %v, want EventFinish", ev.Type)
	}
	if ev.Usage == nil {
		t.Fatal("event[1].Usage is nil")
	}
	if ev.Usage.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42 (from message_start)", ev.Usage.InputTokens)
	}
	if ev.Usage.OutputTokens != 99 {
		t.Errorf("Usage.OutputTokens = %d, want 99 (from message_delta)", ev.Usage.OutputTokens)
	}

	// Third call: io.EOF — the load-bearing signal.
	_, err = stream.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("Next #3 err = %v, want io.EOF", err)
	}
}
