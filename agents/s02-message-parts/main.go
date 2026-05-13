package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// main is a hand-runnable demo of the Part union round-tripping through JSON.
//
//	go run .
//
// builds a fake assistant message ("here's the weather → tool_use → result"),
// marshals it to wire JSON, then unmarshals it back into typed Parts and
// prints each part's Kind + the active variant payload.
func main() {
	original := Message{
		ID:   "msg_demo_1",
		Role: RoleAssistant,
		Content: []Part{
			{Kind: PartText, Text: &TextRef{Text: "Sure — let me check the weather."}},
			{Kind: PartToolUse, ToolUse: &ToolUseRef{
				ID:   "toolu_01abc",
				Name: "get_weather",
				Input: map[string]any{
					"location": "San Francisco, CA",
					"unit":     "celsius",
				},
			}},
			{Kind: PartReasoning, Reasoning: &ReasoningRef{
				Text: "User asked about weather. Calling get_weather tool.",
			}},
		},
	}

	wire, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println("--- wire JSON ---")
	fmt.Println(string(wire))

	var roundtrip Message
	if err := json.Unmarshal(wire, &roundtrip); err != nil {
		fmt.Fprintln(os.Stderr, "unmarshal:", err)
		os.Exit(1)
	}

	fmt.Println("\n--- decoded parts (typed) ---")
	for i, p := range roundtrip.Content {
		switch p.Kind {
		case PartText:
			fmt.Printf("  [%d] text: %q\n", i, p.Text.Text)
		case PartToolUse:
			fmt.Printf("  [%d] tool_use: id=%s name=%s input=%v\n",
				i, p.ToolUse.ID, p.ToolUse.Name, p.ToolUse.Input)
		case PartToolResult:
			fmt.Printf("  [%d] tool_result: id=%s is_error=%v content=%q\n",
				i, p.ToolResult.ToolUseID, p.ToolResult.IsError, p.ToolResult.Content)
		case PartFile:
			fmt.Printf("  [%d] file: %s (%s)\n", i, p.File.Filename, p.File.MediaType)
		case PartReasoning:
			fmt.Printf("  [%d] reasoning: %q\n", i, p.Reasoning.Text)
		case PartSnapshot:
			fmt.Printf("  [%d] snapshot: %s\n", i, p.Snapshot.Snapshot)
		case PartPatch:
			fmt.Printf("  [%d] patch: %s (%d files)\n", i, p.Patch.Hash, len(p.Patch.Files))
		case PartUnknown:
			fmt.Printf("  [%d] unknown: %s\n", i, string(p.Raw))
		}
	}
}
