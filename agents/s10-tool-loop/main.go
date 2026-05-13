package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// main is a hand-runnable demo of the Orchestrator:
//
//	go run .
//
// Builds an Orchestrator with one tool (echo) + one allow rule, and a
// scripted Provider that emits a 2-iteration conversation:
//
//	iteration 1: assistant says "calling echo" + tool_use(echo, "hi")
//	             → Orchestrator runs echo("hi") → "hi"
//	             → emits user Message with tool_result("hi")
//
//	iteration 2: assistant says "echo returned: hi. done." (no tool_use)
//	             → loop terminates naturally.
//
// Output: the full Message trail (4 messages) marshaled as JSON. No
// network, no env vars, no I/O — same deterministic shape as s06's demo.
func main() {
	tools := NewRegistry()
	if err := tools.Register(EchoTool{}); err != nil {
		log.Fatalf("register echo: %v", err)
	}

	provider := &fakeProvider{
		scripts: [][]Event{
			// iteration 1
			{
				{Type: EventText, Text: "I'll call echo: "},
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID:    "toolu_demo_1",
					Name:  "echo",
					Input: json.RawMessage(`{"text":"hi from the demo"}`),
				}},
				{Type: EventFinish, Usage: &Usage{InputTokens: 8, OutputTokens: 6}},
			},
			// iteration 2 — assistant reads the tool result, summarizes,
			// and ends. No tool_use → Orchestrator returns.
			{
				{Type: EventText, Text: "echo returned: hi from the demo. "},
				{Type: EventText, Text: "done."},
				{Type: EventFinish, Usage: &Usage{InputTokens: 14, OutputTokens: 9}},
			},
		},
	}

	orch := &Orchestrator{
		Provider: provider,
		Tools:    tools,
		Permissions: Ruleset{
			{Permission: "*", Pattern: "*", Action: ActionAllow},
		},
		MaxIterations: 5, // generous cap; the natural end_turn fires first
	}

	initial := []Message{
		{Role: RoleUser, Parts: []Part{
			{Kind: PartText, Text: &TextPart{Text: "echo 'hi from the demo'"}},
		}},
	}

	trail, err := orch.Run(context.Background(), initial)
	if err != nil {
		log.Fatalf("orch.Run: %v", err)
	}

	out, err := json.MarshalIndent(trail, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	fmt.Fprintf(os.Stdout, "\ntrail length: %d (initial=1 + 2 iterations × 2 messages = 5? actually %d because the last assistant has no tool_results)\n", len(trail), len(trail))
	fmt.Fprintf(os.Stdout, "Provider.Stream calls: %d\n", provider.callCount)
}
