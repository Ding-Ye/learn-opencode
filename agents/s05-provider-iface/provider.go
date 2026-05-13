package main

import (
	"context"
	"encoding/json"
)

// Provider is the s05 abstraction over a streaming LLM call. It is
// deliberately the *streaming* shape — every later session (s06's loop, s10's
// tool-execution loop, s14's retry wrapper) consumes Events incrementally.
//
// Mirrors the contract opencode's `packages/opencode/src/provider/provider.ts`
// presents to its consumer (`session/llm.ts`): give a request describing
// (model, system, messages, tools), get back something you can iterate to
// receive text chunks, tool calls, reasoning, and a final usage tally.
//
// The s01 version of this interface was `CreateMessage(ctx, req) (*Resp, error)`
// — one round trip, one full response. That worked for "smoke-test the wire
// format" and nothing else: Anthropic streams tool_use across many SSE frames,
// and a real loop has to make permission decisions mid-stream. So s05 swaps
// the return type for a Stream we pull from until io.EOF.
//
// Phase G (multi-model addendum) will add `OpenAIProvider`, `BedrockProvider`,
// etc. — each is a struct that satisfies this interface and translates its
// vendor's wire format into our Event union. The interface stays unchanged.
type Provider interface {
	// Stream sends `req` to the underlying LLM and returns a Stream the
	// caller pulls Events from. Implementations MUST NOT block until the
	// full response is received; the whole point is that Next() returns
	// each Event as it arrives over the wire.
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Request is the provider-agnostic shape of one LLM invocation. Each
// Provider impl translates this into its vendor's wire format
// (Anthropic /v1/messages here; OpenAI /v1/chat/completions in Phase G).
//
// We carry only the fields opencode's runtime actually uses; provider-specific
// knobs (temperature presets, top_p, JSON mode, tool_choice biasing) are
// deliberately absent — they belong on a future per-provider Options struct
// and would otherwise leak vendor concepts into the interface.
type Request struct {
	// Model is the vendor's model ID (e.g. "claude-sonnet-4-5-20250929").
	// Empty → the Provider impl falls back to the default it was constructed with.
	Model string

	// System is a single system prompt string. Anthropic accepts an
	// array-of-blocks shape too; we collapse to one string for s05.
	System string

	// Messages is the full conversation history (alternating user/assistant).
	Messages []Message

	// Tools is the JSON Schema set the LLM can call. s03 produced these.
	Tools []ToolSchema

	// MaxTokens caps the assistant's response length. 0 → impl default.
	MaxTokens int

	// Temperature in [0, 1]. 0 = deterministic. 0 → impl default.
	Temperature float64
}

// Stream is the "iterator-of-events" the caller pulls from. Next() returns
// one Event at a time until the stream is exhausted, at which point it
// returns io.EOF (the Go-idiomatic "stream done" signal — same as
// `bufio.Scanner.Err() == nil && !Scan()` or `(*os.File).Read` at EOF).
//
// Close MUST be called when the caller is done — even early, even on error
// — so the underlying HTTP connection is released. Wrap in `defer`.
type Stream interface {
	// Next blocks until the next Event arrives, then returns it. When the
	// upstream sends `message_stop` (or the connection ends cleanly), Next
	// returns `(Event{}, io.EOF)`. Any other error means the stream is
	// dead — the caller should Close and not call Next again.
	Next() (Event, error)

	// Close releases the underlying HTTP connection. Idempotent.
	Close() error
}

// EventType discriminates the Event union. Zero value is EventText so a
// freshly-zeroed Event is "an empty text chunk" — harmless if a buggy
// impl returns it, which we'd rather have than a panic-on-switch default.
type EventType int

const (
	// EventText is one text delta. Concatenate all EventText.Text values
	// in arrival order to reconstruct the assistant's prose.
	EventText EventType = iota

	// EventToolUse is a fully-assembled tool_use block. We don't surface
	// per-token deltas of tool input — the impl buffers them until
	// content_block_stop, then emits one EventToolUse with the parsed
	// JSON input. (s06 will revisit this when it streams tool_use to the
	// UI for live "the model is calling X right now" feedback.)
	EventToolUse

	// EventReasoning is one chunk of the model's extended-thinking output
	// (Anthropic interleaved-thinking, OpenAI o1 reasoning). Treat as
	// metadata — never feed back to the model as user input.
	EventReasoning

	// EventFinish is the terminal event before io.EOF. Carries the final
	// Usage tally; consumers (s14) accumulate this into the session row.
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

// Event is one item pulled from a Stream. Only the fields relevant to its
// Type are populated — the rest are zero values. Consumers switch on Type
// and read the corresponding field; ignore any field not named for their case.
type Event struct {
	// Type discriminates which of the below fields is meaningful.
	Type EventType

	// Text is set when Type == EventText.
	Text string

	// ToolUse is set when Type == EventToolUse — the assembled tool call.
	ToolUse *ToolUseRef

	// Reasoning is set when Type == EventReasoning.
	Reasoning string

	// Usage is set when Type == EventFinish — final token tally.
	Usage *Usage
}

// Message is one turn in the conversation. Same wire shape as s01's Message,
// extended for tool_use / tool_result content blocks the streaming protocol
// can produce.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant" | "system"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block inside a Message. Anthropic's API distinguishes
// "text", "tool_use", "tool_result", "thinking" — we tag with Type and only
// fill the field that matches.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`         // tool_use block id
	Name      string          `json:"name,omitempty"`       // tool_use name
	Input     json.RawMessage `json:"input,omitempty"`      // tool_use input
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result reference
	Content   string          `json:"content,omitempty"`    // tool_result content
}

// ToolSchema is one tool the LLM may invoke. Mirrors Anthropic's
// {name, description, input_schema} — the same triple s03 produced from its
// `Tool` interface. Phase G's OpenAI provider translates this into the
// {type:"function", function:{...}} OpenAI nests it inside.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUseRef is what an EventToolUse carries — the assembled-from-deltas
// call site. Same fields as s02's ToolUse Part; we use a distinct name to
// keep s05 self-contained (each session is its own go module, no cross-imports).
type ToolUseRef struct {
	ID    string          // tool_use id from the stream (e.g. "toolu_01ABC")
	Name  string          // tool name the LLM picked (e.g. "edit", "bash")
	Input json.RawMessage // input JSON, fully buffered before this event fires
}

// Usage is the token tally a finish event carries. s14 will extend this with
// reasoning + cache fields; for s05 we keep the two columns every provider
// returns.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
