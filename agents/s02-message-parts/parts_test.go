package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// roundtrip marshals `in`, unmarshals it back, and returns both the wire bytes
// and the decoded value. Every test uses this so failures point at exactly one
// step (encode, decode, or value diff).
func roundtripPart(t *testing.T, in Part) (wire []byte, out Part) {
	t.Helper()
	wire, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(wire, &out); err != nil {
		t.Fatalf("unmarshal: %v (wire=%s)", err, string(wire))
	}
	return wire, out
}

func TestRoundtripTextPart(t *testing.T) {
	in := Part{Kind: PartText, Text: &TextRef{Text: "hello world"}}

	wire, out := roundtripPart(t, in)

	if !strings.Contains(string(wire), `"type":"text"`) {
		t.Errorf("wire missing discriminator: %s", string(wire))
	}
	if !strings.Contains(string(wire), `"text":"hello world"`) {
		t.Errorf("wire missing payload: %s", string(wire))
	}
	if out.Kind != PartText {
		t.Fatalf("Kind = %q, want %q", out.Kind, PartText)
	}
	if out.Text == nil || out.Text.Text != "hello world" {
		t.Errorf("Text payload lost: %+v", out.Text)
	}
}

func TestRoundtripToolUsePart(t *testing.T) {
	in := Part{Kind: PartToolUse, ToolUse: &ToolUseRef{
		ID:   "toolu_01XYZ",
		Name: "get_weather",
		Input: map[string]any{
			"location": "Tokyo",
			"unit":     "celsius",
		},
	}}

	wire, out := roundtripPart(t, in)

	if !strings.Contains(string(wire), `"type":"tool_use"`) {
		t.Errorf("wire missing discriminator: %s", string(wire))
	}
	if out.Kind != PartToolUse {
		t.Fatalf("Kind = %q, want %q", out.Kind, PartToolUse)
	}
	if out.ToolUse == nil {
		t.Fatal("ToolUse payload nil after decode")
	}
	if out.ToolUse.ID != "toolu_01XYZ" || out.ToolUse.Name != "get_weather" {
		t.Errorf("ToolUse fields lost: %+v", out.ToolUse)
	}
	if got := out.ToolUse.Input["location"]; got != "Tokyo" {
		t.Errorf("ToolUse.Input.location = %v, want Tokyo", got)
	}
	if got := out.ToolUse.Input["unit"]; got != "celsius" {
		t.Errorf("ToolUse.Input.unit = %v, want celsius", got)
	}
}

func TestRoundtripToolResultPart(t *testing.T) {
	in := Part{Kind: PartToolResult, ToolResult: &ToolResultRef{
		ToolUseID: "toolu_01XYZ",
		Content:   "22°C, sunny",
		IsError:   false,
	}}

	wire, out := roundtripPart(t, in)

	if !strings.Contains(string(wire), `"type":"tool_result"`) {
		t.Errorf("wire missing discriminator: %s", string(wire))
	}
	// IsError=false should be omitted by `,omitempty`.
	if strings.Contains(string(wire), `"is_error"`) {
		t.Errorf("is_error=false should be omitted, got %s", string(wire))
	}
	if out.Kind != PartToolResult {
		t.Fatalf("Kind = %q, want %q", out.Kind, PartToolResult)
	}
	if out.ToolResult == nil {
		t.Fatal("ToolResult payload nil after decode")
	}
	if out.ToolResult.ToolUseID != "toolu_01XYZ" {
		t.Errorf("ToolUseID lost: %q", out.ToolResult.ToolUseID)
	}
	if out.ToolResult.Content != "22°C, sunny" {
		t.Errorf("Content lost: %q", out.ToolResult.Content)
	}

	// Now flip is_error=true and confirm it does land on the wire.
	in2 := Part{Kind: PartToolResult, ToolResult: &ToolResultRef{
		ToolUseID: "toolu_err",
		Content:   "boom",
		IsError:   true,
	}}
	wire2, out2 := roundtripPart(t, in2)
	if !strings.Contains(string(wire2), `"is_error":true`) {
		t.Errorf("is_error=true missing from wire: %s", string(wire2))
	}
	if out2.ToolResult == nil || !out2.ToolResult.IsError {
		t.Errorf("is_error didn't roundtrip: %+v", out2.ToolResult)
	}
}

func TestRoundtripMixedMessage(t *testing.T) {
	msg := Message{
		ID:   "msg_01",
		Role: RoleAssistant,
		Content: []Part{
			{Kind: PartText, Text: &TextRef{Text: "Calling tool now."}},
			{Kind: PartToolUse, ToolUse: &ToolUseRef{
				ID: "toolu_X", Name: "ping", Input: map[string]any{"host": "example.com"},
			}},
		},
	}

	wire, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v (wire=%s)", err, string(wire))
	}

	if got.ID != "msg_01" || got.Role != RoleAssistant {
		t.Errorf("envelope lost: %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(got.Content))
	}
	if got.Content[0].Kind != PartText {
		t.Errorf("first part Kind = %q", got.Content[0].Kind)
	}
	if got.Content[1].Kind != PartToolUse {
		t.Errorf("second part Kind = %q", got.Content[1].Kind)
	}
	if got.Content[1].ToolUse == nil || got.Content[1].ToolUse.Name != "ping" {
		t.Errorf("tool_use payload lost: %+v", got.Content[1].ToolUse)
	}
	// Order matters — opencode's processor.ts depends on Parts being a
	// strict sequence (Tool follows Text follows Reasoning). Confirm it
	// here so the union doesn't quietly become a set.
	if got.Content[0].Kind == got.Content[1].Kind {
		t.Errorf("part order collapsed")
	}
}

func TestUnknownPartKindDecodesAsUnknown(t *testing.T) {
	// A future opencode release adds a "compaction" part type. We must NOT
	// panic, NOT error out — just preserve the raw bytes for forward-compat.
	wire := []byte(`{"type":"compaction","auto":true,"overflow":false}`)

	var p Part
	if err := json.Unmarshal(wire, &p); err != nil {
		t.Fatalf("unexpected error decoding unknown kind: %v", err)
	}
	if p.Kind != PartUnknown {
		t.Errorf("Kind = %q, want PartUnknown", p.Kind)
	}
	if len(p.Raw) == 0 {
		t.Fatal("Raw bytes not preserved")
	}
	if !strings.Contains(string(p.Raw), `"compaction"`) {
		t.Errorf("Raw lost original payload: %s", string(p.Raw))
	}

	// Re-marshal: the unknown should still survive a full round-trip.
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("re-marshal failed: %v", err)
	}
	if !strings.Contains(string(out), `"compaction"`) {
		t.Errorf("re-marshal dropped original payload: %s", string(out))
	}
}
