package main

import (
	"context"
	"encoding/json"
)

// Provider, Stream, Event — re-implemented from s05 verbatim. Each session is
// its own Go module with no cross-session imports, so the contract that s05
// established (one method, returns a pull-based iterator of Events) is
// reproduced here. s06's job is to *consume* a Stream, not to define a new one
// — the interface is unchanged precisely so the Loop below works against any
// future Provider (Phase G's OpenAIProvider, BedrockProvider, …).
type Provider interface {
	// Stream sends `req` to the underlying LLM and returns a Stream the
	// caller pulls Events from. Implementations MUST NOT block until the
	// full response is received — Next() returns each Event as it arrives.
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Request is the cross-vendor LLM invocation. Carried verbatim from s05.
type Request struct {
	Model       string
	System      string
	Messages    []ProviderMessage
	Tools       []ToolSchema
	MaxTokens   int
	Temperature float64
}

// Stream is the iterator-of-events the caller pulls from. Carried from s05.
//
// Next() returns one Event at a time until the upstream sends its terminal
// signal, at which point it returns (Event{}, io.EOF). Close MUST be called
// — even early, even on error — so the underlying connection is released.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// EventType discriminates the Event union. Carried from s05 — same iota order
// (Text=0 stays the zero value so a buggy impl can't accidentally tag a
// non-text event as text).
type EventType int

const (
	// EventText is one text delta. Concatenate all EventText.Text values
	// in arrival order to reconstruct the assistant's prose.
	EventText EventType = iota

	// EventToolUse is a fully-assembled tool_use block — input JSON is
	// already buffered and parsed by the time this Event fires.
	EventToolUse

	// EventReasoning is one chunk of extended-thinking output.
	EventReasoning

	// EventFinish is the terminal event before io.EOF. Carries final Usage.
	EventFinish
)

// String makes EventType printable in test failures and logs.
func (t EventType) String() string {
	switch t {
	case EventToolUse:
		return "tool_use"
	case EventReasoning:
		return "reasoning"
	case EventFinish:
		return "finish"
	default:
		return "text"
	}
}

// Event is one item pulled from a Stream. Only the field matching Type is
// populated; the rest are zero values. Consumers switch on Type and read the
// corresponding field.
type Event struct {
	Type      EventType
	Text      string
	ToolUse   *ToolUseEvent // set when Type == EventToolUse
	Reasoning string        // set when Type == EventReasoning
	Usage     *Usage        // set when Type == EventFinish
}

// ProviderMessage is one turn in the conversation as seen by the Provider
// interface — it's the wire-shape Message the s05 Anthropic impl posts to
// /v1/messages. We give it a distinct name from `Message` (the assembled-by-Loop
// message-of-Parts below) so the two layers don't collide; in s10 the Loop
// will translate one into the other when feeding tool results back.
type ProviderMessage struct {
	Role    string         `json:"role"`    // "user" | "assistant" | "system"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block inside a ProviderMessage. Same shape as s05.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// ToolSchema is one tool the LLM may invoke. Same shape as s05.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUseEvent is what an EventToolUse carries — the assembled-from-deltas
// call site, with input fully buffered. Distinct from ToolUseRef in parts.go
// (the persisted Part variant) because they live in two different layers:
// this one is "what the wire sent us right now"; ToolUseRef is "what we glued
// onto the assembled Message after the fact." Most fields overlap; in s10 the
// Loop will copy ToolUseEvent → ToolUseRef when appending to the Message.
type ToolUseEvent struct {
	ID    string          // tool_use id from the stream (e.g. "toolu_01ABC")
	Name  string          // tool name the LLM picked
	Input json.RawMessage // input JSON, fully buffered before this event fires
}

// Usage is the token tally a finish event carries. s14 will extend with
// reasoning + cache fields; s06 keeps the two columns every provider returns.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
