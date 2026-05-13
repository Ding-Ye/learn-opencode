package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// main is a hand-runnable demo of the Loop:
//
//	go run .
//
// Builds a fakeProvider with a small scripted stream (two text deltas, one
// tool_use, one final text, one finish), then calls Loop.Consume and prints
// the assembled Message as JSON. Output is deterministic and self-contained
// — no network, no API key — so this also serves as a smoke test you can run
// before `go test`.
//
// In s10 the demo will be replaced by one that pipes a real
// AnthropicProvider into the Loop and dispatches tool calls; the Consume()
// call site stays unchanged.
func main() {
	// Scripted stream: same shape s05's tool_use test produced, except
	// here we feed the Loop directly with Events instead of going through
	// SSE bytes. The end result is the same Message you'd get from a real
	// LLM that said "Here is the result: <call edit a.go>; done."
	provider := &fakeProvider{
		events: []Event{
			{Type: EventText, Text: "Here is the result: "},
			{Type: EventText, Text: "calling edit "},
			{Type: EventToolUse, ToolUse: &ToolUseEvent{
				ID:    "toolu_demo_1",
				Name:  "edit",
				Input: json.RawMessage(`{"path":"a.go","old":"x","new":"y"}`),
			}},
			{Type: EventText, Text: "done."},
			{Type: EventFinish, Usage: &Usage{InputTokens: 12, OutputTokens: 17}},
		},
	}

	loop := &Loop{Provider: provider}

	msg, err := loop.Consume(context.Background(), Request{
		Model: "demo-model",
		Messages: []ProviderMessage{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "edit a.go"}}},
		},
	})
	if err != nil {
		log.Fatalf("loop.Consume: %v", err)
	}

	// Pretty-print the assembled Message. Note how the two adjacent text
	// EventTexts collapsed into ONE PartText, separated from the trailing
	// "done." text by the tool_use Part — same boundary rule s10 will
	// rely on for "did the model finish or did it ask for a tool?"
	out, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
}
