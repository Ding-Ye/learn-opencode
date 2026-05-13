package main

import "encoding/json"

// Part / Message — re-implemented from s02 / s06. s10 needs them because
// the Orchestrator's loop produces NEW messages between Provider rounds:
//
//   1. The assistant Message assembled from the stream (Parts: text +
//      tool_use, possibly reasoning).
//   2. A USER Message with one tool_result Part per tool_use the previous
//      assistant Message asked for — this is how the next Provider call
//      learns what each tool returned.
//
// Each session is its own Go module — no cross-imports — so the wire shape
// is duplicated here. It matches s02 / s06 byte-for-byte; an s06-encoded
// Message would round-trip through this code unchanged.

// PartKind discriminates the Part tagged union. Same constants as s02 / s06.
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

// Message is one turn in the assembled conversation. The Orchestrator's
// Run produces an alternating sequence: assistant (from Provider stream)
// → user (with tool_result Parts) → assistant → user → ... until the
// assistant message contains no tool_use Parts.
//
// We carry StopReason and Usage from s06 — both informational, both
// preserved for the test that pins the loop-exits-on-end_turn contract.
type Message struct {
	ID         string `json:"id,omitempty"`
	Role       Role   `json:"role"`
	Parts      []Part `json:"parts"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
}

// Part is the tagged union. s10 is the first session that produces ALL FOUR
// variants (s06 only produced text/tool_use/reasoning; s02 only consumed
// them). The user-Message-with-tool_results path here is the first place a
// PartToolResult is constructed in the curriculum.
type Part struct {
	Kind       PartKind
	Text       *TextPart       // Kind == PartText
	ToolUse    *ToolUsePart    // Kind == PartToolUse
	ToolResult *ToolResultPart // Kind == PartToolResult
	Reasoning  *ReasoningPart  // Kind == PartReasoning
}

// TextPart is one assembled prose block — the result of concatenating all
// adjacent EventText.Text values from the stream.
type TextPart struct {
	Text string `json:"text"`
}

// ToolUsePart is one assembled tool_use call site. The Orchestrator walks
// the assistant Message's Parts looking for these — each one drives one
// Permission check + Tool execution + tool_result emission.
type ToolUsePart struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultPart is one tool execution result. s10 produces these in the
// post-stream feedback step:
//   - Permission denied → ToolResultPart{IsError: true, Content: "permission denied: ..."}
//   - Tool returned err → ToolResultPart{IsError: true, Content: "tool error: ..."}
//   - Tool returned ok  → ToolResultPart{IsError: false, Content: <output>}
type ToolResultPart struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ReasoningPart is one assembled extended-thinking block. Concatenated
// across all adjacent EventReasoning chunks. s10 doesn't act on these but
// preserves them so a downstream consumer (s07 persist, s14 cost) sees
// them in arrival order.
type ReasoningPart struct {
	Text string `json:"text"`
}

// MarshalJSON projects a Part to its flat wire shape — same algorithm as
// s06. Lets the demo's `main.go` print the assembled message trail as JSON
// without the test-only Kind discriminator field leaking out.
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
