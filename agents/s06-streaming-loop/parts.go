package main

import "encoding/json"

// Part / Message — re-implemented from s02 (same wire shape). s06 needs them
// because the Loop's job is precisely to assemble Events into a Message of
// Parts: a stream of N text deltas collapses to ONE text Part; a stream of M
// input_json_delta frames collapses to ONE tool_use Part; a thinking stream
// to ONE reasoning Part. The Loop is the bridge between the Stream layer
// (Events as they arrive over the wire) and the Message layer (what gets
// persisted to a session and fed back to the next LLM call).
//
// Each session is its own Go module — no cross-imports — so we duplicate the
// types here. The wire format matches s02 byte-for-byte; an s02-encoded
// Message would round-trip through this code unchanged.

// PartKind discriminates the Part tagged union. Same constants as s02.
type PartKind string

const (
	PartUnknown    PartKind = ""
	PartText       PartKind = "text"
	PartToolUse    PartKind = "tool_use"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
)

// Role discriminates Message.Role. Same constants opencode uses.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Message is one turn in the assembled conversation. The Loop builds an
// assistant Message from a stream of Events; s07 will persist it; s10 will
// extend the slice with the next user/assistant pair.
//
// Distinct from ProviderMessage in provider.go: that one is the wire shape
// the LLM API speaks; this one is the in-memory Part list the rest of the
// agent operates on. The two are convertible (one assistant Message of
// Parts → one ProviderMessage of ContentBlocks) and s10 will write the
// translator. For s06 we only need the Loop direction: Events → this.
type Message struct {
	ID         string  `json:"id,omitempty"`
	Role       Role    `json:"role"`
	Parts      []Part  `json:"parts"`
	StopReason string  `json:"stop_reason,omitempty"`
	Usage      *Usage  `json:"usage,omitempty"`
}

// Part is the tagged union. Carried from s02 with the variant payloads s06
// actually needs (Text / ToolUse / Reasoning) — no FilePart / SnapshotPart
// because the streaming layer doesn't produce those (s07 / s10 will).
//
// Naming note: the variant payload types are `TextPart`, `ToolUsePart`,
// etc. (suffix-Part), distinct from the `PartText` PartKind constant
// (prefix-Part). Same convention the s02 module uses to avoid the name
// collision Go won't let you have between a struct and a const in the
// same package.
type Part struct {
	Kind       PartKind
	Text       *TextPart        // Kind == PartText
	ToolUse    *ToolUsePart     // Kind == PartToolUse
	ToolResult *ToolResultPart  // Kind == PartToolResult
	Reasoning  *ReasoningPart   // Kind == PartReasoning
}

// TextPart is one assembled prose block — the result of concatenating all
// adjacent EventText.Text values from the stream.
type TextPart struct {
	Text string `json:"text"`
}

// ToolUsePart is one assembled tool_use call site. Distinct name from the
// ToolUseEvent in provider.go (which is what the wire delivers); this one is
// the persisted Part variant. They carry the same data; the Loop copies one
// into the other.
type ToolUsePart struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultPart is one tool execution result. s06 doesn't produce these —
// the Loop only consumes the assistant stream — but the Part union must
// carry the kind so s10's tool loop can append results to the next user
// message without reshaping Part.
type ToolResultPart struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ReasoningPart is one assembled extended-thinking block. Concatenated across
// all adjacent EventReasoning chunks.
type ReasoningPart struct {
	Text string `json:"text"`
}

// MarshalJSON projects a Part to its flat wire shape — `{"type":"text","text":"..."}`
// — picking the active variant based on Kind. Same algorithm as s02; copied
// here verbatim so s06 can serialize an assembled Message for inspection
// (the demo's `main.go` prints the result as JSON).
func (p Part) MarshalJSON() ([]byte, error) {
	switch p.Kind {
	case PartText:
		if p.Text == nil {
			return json.Marshal(map[string]any{"type": "text", "text": ""})
		}
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", p.Text.Text})
	case PartToolUse:
		if p.ToolUse == nil {
			return json.Marshal(map[string]any{"type": "tool_use"})
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{"tool_use", p.ToolUse.ID, p.ToolUse.Name, p.ToolUse.Input})
	case PartToolResult:
		if p.ToolResult == nil {
			return json.Marshal(map[string]any{"type": "tool_result"})
		}
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error,omitempty"`
		}{"tool_result", p.ToolResult.ToolUseID, p.ToolResult.Content, p.ToolResult.IsError})
	case PartReasoning:
		if p.Reasoning == nil {
			return json.Marshal(map[string]any{"type": "reasoning", "text": ""})
		}
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"reasoning", p.Reasoning.Text})
	default:
		return json.Marshal(map[string]any{"type": string(p.Kind)})
	}
}
