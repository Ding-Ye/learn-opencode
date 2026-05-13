package main

import (
	"encoding/json"
	"fmt"
)

// PartKind is the discriminator of the Part tagged union. It mirrors the
// `type:` field on every Part variant in opencode's
// packages/opencode/src/session/message-v2.ts, but kept to the subset s02
// actually wires up. Later sessions add more kinds without breaking JSON
// compatibility — they only widen the switch in MarshalJSON / UnmarshalJSON.
type PartKind string

const (
	PartUnknown    PartKind = ""
	PartText       PartKind = "text"
	PartToolUse    PartKind = "tool_use"
	PartToolResult PartKind = "tool_result"
	PartFile       PartKind = "file"
	PartReasoning  PartKind = "reasoning"
	PartSnapshot   PartKind = "snapshot"
	PartPatch      PartKind = "patch"
)

// Part is a tagged union: exactly one of the *Ref pointers below is non-nil
// for any non-Unknown Kind. Go has no real sum types, so we lean on JSON
// custom marshalling to project Part to / from the wire shape that Anthropic
// (and opencode) speak: `{"type":"text","text":"..."}` style.
//
// The choice to keep one Go struct (rather than one Go type per variant + an
// `interface{ Kind() PartKind }`) is on purpose: the LLM wire format is a
// flat union, the consumer-side switch is small (≤ 7 arms), and a single
// struct trivially survives `[]Part` round-trip without an unmarshaller-side
// dispatcher per slot. opencode's TypeScript version uses a `Schema.Union`
// over labelled structs — same shape, different language idiom.
type Part struct {
	Kind PartKind

	Text       *TextRef       // Kind == PartText
	ToolUse    *ToolUseRef    // Kind == PartToolUse
	ToolResult *ToolResultRef // Kind == PartToolResult
	File       *FileRef       // Kind == PartFile
	Reasoning  *ReasoningRef  // Kind == PartReasoning
	Snapshot   *SnapshotRef   // Kind == PartSnapshot
	Patch      *PatchRef      // Kind == PartPatch

	// Raw is set when UnmarshalJSON encounters a `type` we don't recognize.
	// We keep the original payload so a forward-compat consumer can still
	// see it instead of crashing — opencode's wire format is a moving
	// target as new part kinds (compaction, retry, step-start...) land.
	Raw json.RawMessage
}

// --- variant payloads ---

type TextRef struct {
	Text string `json:"text"`
}

type ToolUseRef struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type ToolResultRef struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type FileRef struct {
	MediaType string `json:"media_type"`
	URL       string `json:"url,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

type ReasoningRef struct {
	Text string `json:"text"`
}

type SnapshotRef struct {
	Snapshot string `json:"snapshot"`
}

type PatchRef struct {
	Hash  string   `json:"hash"`
	Files []string `json:"files"`
}

// --- JSON wire format ---
//
// On the wire, every Part is a flat object with a discriminator field:
//
//   {"type":"text",       "text": "..."}
//   {"type":"tool_use",   "id":"...", "name":"...", "input":{...}}
//   {"type":"tool_result","tool_use_id":"...", "content":"...", "is_error":false}
//
// MarshalJSON picks the active *Ref based on Kind and inlines its fields
// alongside `type`. UnmarshalJSON reads `type` first, then routes the rest
// of the bytes into the matching *Ref.

// MarshalJSON renders the Part as `{"type":<kind>, ...payload fields}`.
func (p Part) MarshalJSON() ([]byte, error) {
	switch p.Kind {
	case PartText:
		return mergeTyped(string(p.Kind), p.Text)
	case PartToolUse:
		return mergeTyped(string(p.Kind), p.ToolUse)
	case PartToolResult:
		return mergeTyped(string(p.Kind), p.ToolResult)
	case PartFile:
		return mergeTyped(string(p.Kind), p.File)
	case PartReasoning:
		return mergeTyped(string(p.Kind), p.Reasoning)
	case PartSnapshot:
		return mergeTyped(string(p.Kind), p.Snapshot)
	case PartPatch:
		return mergeTyped(string(p.Kind), p.Patch)
	case PartUnknown:
		// Unknown was set from a payload we couldn't identify on decode —
		// preserve it byte-for-byte so the marshal/unmarshal/marshal cycle
		// is a fixed-point even on forward-compat input.
		if len(p.Raw) > 0 {
			return p.Raw, nil
		}
		return nil, fmt.Errorf("part with PartUnknown has no Raw payload to emit")
	}
	return nil, fmt.Errorf("part: unhandled kind %q", p.Kind)
}

// UnmarshalJSON peeks at `type`, then decodes the rest into the matching ref.
func (p *Part) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("part: read type: %w", err)
	}
	p.Kind = PartKind(probe.Type)

	switch p.Kind {
	case PartText:
		p.Text = new(TextRef)
		return json.Unmarshal(data, p.Text)
	case PartToolUse:
		p.ToolUse = new(ToolUseRef)
		return json.Unmarshal(data, p.ToolUse)
	case PartToolResult:
		p.ToolResult = new(ToolResultRef)
		return json.Unmarshal(data, p.ToolResult)
	case PartFile:
		p.File = new(FileRef)
		return json.Unmarshal(data, p.File)
	case PartReasoning:
		p.Reasoning = new(ReasoningRef)
		return json.Unmarshal(data, p.Reasoning)
	case PartSnapshot:
		p.Snapshot = new(SnapshotRef)
		return json.Unmarshal(data, p.Snapshot)
	case PartPatch:
		p.Patch = new(PatchRef)
		return json.Unmarshal(data, p.Patch)
	default:
		// Unknown discriminator: stash the raw bytes, mark as Unknown, and
		// keep going. Lets the rest of the message decode without the whole
		// stream blowing up on one part the agent didn't introduce yet.
		p.Kind = PartUnknown
		p.Raw = append(p.Raw[:0], data...)
		return nil
	}
}

// mergeTyped inlines `payload` into a JSON object alongside {"type":kind}.
// We could nest payload under a "data" key, but Anthropic's wire format is
// flat — sticking to that means every later session's HTTP code path is the
// same shape, with no envelope/peel step.
func mergeTyped(kind string, payload any) ([]byte, error) {
	if payload == nil {
		// e.g. a Text part with nil TextRef — emit just the type.
		return json.Marshal(map[string]string{"type": kind})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// payload is always a struct → marshals to `{...}`; splice in `"type":...`
	// at the top by reopening the brace. Two-allocation but readable.
	if len(body) < 2 || body[0] != '{' {
		return nil, fmt.Errorf("part payload didn't marshal to a JSON object: %s", string(body))
	}
	if string(body) == "{}" {
		return []byte(fmt.Sprintf(`{"type":%q}`, kind)), nil
	}
	prefix := fmt.Sprintf(`{"type":%q,`, kind)
	out := make([]byte, 0, len(prefix)+len(body)-1)
	out = append(out, prefix...)
	out = append(out, body[1:]...) // body[1:] strips the leading '{'
	return out, nil
}
