package main

import (
	"context"
	"encoding/json"
)

// Provider, Stream, Event — re-implemented from s05 / s06 verbatim. The
// Orchestrator below is the FIRST consumer in the curriculum that calls
// Provider.Stream MORE THAN ONCE per Run: each "iteration" of the tool
// loop is one Stream call. The fake Provider below scripts a *slice of
// Event slices* — one per iteration — so a test can express "first turn
// asks for echo; after the tool result comes back, second turn ends with
// plain text" as data, not control flow.
type Provider interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Request is the cross-vendor LLM invocation. Carried verbatim from s06.
// The Orchestrator rewrites Request.Messages between iterations: every
// new assistant Message + the synthesized user Message of tool_results
// gets appended (translated through messageToProvider in loop.go).
type Request struct {
	Model       string
	System      string
	Messages    []ProviderMessage
	Tools       []ToolSchema
	MaxTokens   int
	Temperature float64
}

// Stream is the iterator-of-events the caller pulls from. Carried from s06.
type Stream interface {
	Next() (Event, error)
	Close() error
}

// EventType discriminates the Event union — same iota order as s06.
type EventType int

const (
	EventText EventType = iota
	EventToolUse
	EventReasoning
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

// Event is one item pulled from a Stream. Same shape as s06.
type Event struct {
	Type      EventType
	Text      string
	ToolUse   *ToolUseEvent // set when Type == EventToolUse
	Reasoning string        // set when Type == EventReasoning
	Usage     *Usage        // set when Type == EventFinish
}

// ProviderMessage is one turn in the conversation as seen by the Provider
// interface — the wire-shape Message the LLM API consumes. Distinct from
// the in-memory Message-of-Parts in parts.go; messageToProvider in loop.go
// is the one place the two shapes meet.
type ProviderMessage struct {
	Role    string         `json:"role"` // "user" | "assistant" | "system"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block inside a ProviderMessage. Same shape as s06,
// extended only by what s10 actually populates (tool_use_id + content for
// the user-side tool_result blocks).
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ToolSchema is one tool the LLM may invoke. Same shape as s06.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUseEvent — what an EventToolUse carries. Same shape as s06.
type ToolUseEvent struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Usage is the token tally a finish event carries — same fields as s06.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
