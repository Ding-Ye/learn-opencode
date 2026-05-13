package main

import "encoding/json"

// PartKind / Part / Message — re-implemented from s02 (same wire shape) so
// s07 can persist the same Part union to SQLite. Each session is its own Go
// module with no cross-session imports, so we duplicate the types verbatim.
//
// The shape s07 actually persists: a Message has many Parts; each Part has
// one Kind and (depending on Kind) one populated variant payload. The DB
// stores Kind in its own column (so queries like "find tool_use Parts in
// session X" don't have to JSON-scan), and the variant payload is one JSON
// blob in the `payload` column. Same trick the upstream session.sql.ts uses
// — `data text({ mode: "json" })`.
type PartKind string

const (
	PartUnknown    PartKind = ""
	PartText       PartKind = "text"
	PartToolUse    PartKind = "tool_use"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
	PartFile       PartKind = "file"
)

// Role discriminates Message.Role. Same constants opencode uses.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Message is one turn in the assembled conversation. s07's job is to persist
// these — one Message row, plus one Part row per Part — and to load them
// back in created_at order with their Parts in position order.
//
// The ID is assigned by the caller (typically a ULID). CreatedAt is set by
// AppendMessage if zero; persisted as Unix milliseconds so Go can compare
// across timezones without a parser dance.
type Message struct {
	ID        string `json:"id,omitempty"`
	Role      Role   `json:"role"`
	Parts     []Part `json:"parts"`
	CreatedAt int64  `json:"created_at,omitempty"` // Unix ms
}

// Part is the tagged union. Same shape as s02; the variant payloads s07
// persists are TextPart / ToolUsePart / ToolResultPart / ReasoningPart /
// FilePart. The DB row carries (Kind, JSON-of-variant) — load-time we
// re-hydrate the matching pointer based on Kind.
type Part struct {
	ID         string          `json:"id,omitempty"`
	Kind       PartKind        `json:"-"`
	Text       *TextPart       `json:"text,omitempty"`
	ToolUse    *ToolUsePart    `json:"tool_use,omitempty"`
	ToolResult *ToolResultPart `json:"tool_result,omitempty"`
	Reasoning  *ReasoningPart  `json:"reasoning,omitempty"`
	File       *FilePart       `json:"file,omitempty"`
}

// TextPart is one assembled prose block.
type TextPart struct {
	Text string `json:"text"`
}

// ToolUsePart is one tool_use call site (assembled, with input fully
// buffered before persistence).
type ToolUsePart struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultPart is one tool execution result.
type ToolResultPart struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ReasoningPart is one assembled extended-thinking block.
type ReasoningPart struct {
	Text string `json:"text"`
}

// FilePart is one referenced file (e.g. an image attached to a user
// message). s07 only persists the metadata; bytes live elsewhere.
type FilePart struct {
	MediaType string `json:"media_type"`
	URL       string `json:"url,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

// payloadJSON returns the JSON bytes for the active variant — what the DB
// stores in the `payload` column. The Kind discriminator is stored in its
// own column, so this only needs to encode the variant's fields.
func (p Part) payloadJSON() ([]byte, error) {
	switch p.Kind {
	case PartText:
		if p.Text == nil {
			return json.Marshal(&TextPart{})
		}
		return json.Marshal(p.Text)
	case PartToolUse:
		if p.ToolUse == nil {
			return json.Marshal(&ToolUsePart{})
		}
		return json.Marshal(p.ToolUse)
	case PartToolResult:
		if p.ToolResult == nil {
			return json.Marshal(&ToolResultPart{})
		}
		return json.Marshal(p.ToolResult)
	case PartReasoning:
		if p.Reasoning == nil {
			return json.Marshal(&ReasoningPart{})
		}
		return json.Marshal(p.Reasoning)
	case PartFile:
		if p.File == nil {
			return json.Marshal(&FilePart{})
		}
		return json.Marshal(p.File)
	default:
		// Forward-compat: an unrecognized Kind round-trips as an empty
		// object so loaders don't crash on a future-introduced Kind.
		return []byte("{}"), nil
	}
}

// partFromRow re-hydrates a Part from a (Kind, payload) DB row. The
// inverse of payloadJSON. We populate exactly one variant pointer based
// on Kind so downstream code can keep using the same tagged-union shape
// it built in memory.
func partFromRow(id string, kind PartKind, payload []byte) (Part, error) {
	p := Part{ID: id, Kind: kind}
	if len(payload) == 0 {
		return p, nil
	}
	switch kind {
	case PartText:
		v := &TextPart{}
		if err := json.Unmarshal(payload, v); err != nil {
			return p, err
		}
		p.Text = v
	case PartToolUse:
		v := &ToolUsePart{}
		if err := json.Unmarshal(payload, v); err != nil {
			return p, err
		}
		p.ToolUse = v
	case PartToolResult:
		v := &ToolResultPart{}
		if err := json.Unmarshal(payload, v); err != nil {
			return p, err
		}
		p.ToolResult = v
	case PartReasoning:
		v := &ReasoningPart{}
		if err := json.Unmarshal(payload, v); err != nil {
			return p, err
		}
		p.Reasoning = v
	case PartFile:
		v := &FilePart{}
		if err := json.Unmarshal(payload, v); err != nil {
			return p, err
		}
		p.File = v
	default:
		// Unrecognized Kind — leave variants nil. Same forward-compat
		// stance as the s02 Part.UnmarshalJSON Raw-stash.
	}
	return p, nil
}
