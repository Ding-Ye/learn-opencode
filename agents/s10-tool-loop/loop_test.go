package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestZeroToolConversationCompletes pins the simplest path: the assistant
// answers in plain text, no tool_use, the loop terminates after ONE
// Provider.Stream call. This is the load-bearing baseline — every more
// elaborate test below adds tool_use rounds on top of this skeleton.
//
// If a refactor accidentally made Run loop forever even when the
// assistant emitted no tool calls, this test would hang or hit an
// MaxIterations cap. We pin both: exactly 1 stream call AND trail =
// initial(1) + assistant(1) = 2 messages.
func TestZeroToolConversationCompletes(t *testing.T) {
	provider := &fakeProvider{
		scripts: [][]Event{
			{
				{Type: EventText, Text: "Hello, no tools needed."},
				{Type: EventFinish, Usage: &Usage{InputTokens: 3, OutputTokens: 5}},
			},
		},
	}
	orch := &Orchestrator{
		Provider:      provider,
		Tools:         NewRegistry(),
		MaxIterations: 5,
	}
	initial := []Message{
		{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "hi"}}}},
	}
	trail, err := orch.Run(context.Background(), initial)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount != 1 {
		t.Errorf("Stream calls = %d, want 1", provider.callCount)
	}
	if len(trail) != 2 {
		t.Fatalf("trail length = %d, want 2 (initial + assistant)", len(trail))
	}
	if trail[1].Role != RoleAssistant {
		t.Errorf("trail[1].Role = %q, want assistant", trail[1].Role)
	}
	if trail[1].StopReason != "end_turn" {
		t.Errorf("trail[1].StopReason = %q, want end_turn", trail[1].StopReason)
	}
}

// TestOneToolRoundTrip pins the canonical loop shape: assistant asks for
// echo → Orchestrator runs it → user Message with tool_result → assistant
// summarizes and ends. Trail length = 4 (initial + assistant + user-result
// + assistant), Stream called twice, the tool_result Part carries the
// echo output verbatim.
//
// This is the test that proves the THREE coordination steps are wired
// correctly: Provider.Stream → Tool.Execute → next Provider.Stream sees
// the result. If any of the three is broken, this test catches it.
func TestOneToolRoundTrip(t *testing.T) {
	tools := NewRegistry()
	if err := tools.Register(EchoTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	provider := &fakeProvider{
		scripts: [][]Event{
			// iteration 1: ask for echo
			{
				{Type: EventText, Text: "calling echo"},
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID:    "tu_1",
					Name:  "echo",
					Input: json.RawMessage(`{"text":"round-trip"}`),
				}},
				{Type: EventFinish},
			},
			// iteration 2: assistant ends turn
			{
				{Type: EventText, Text: "got: round-trip"},
				{Type: EventFinish},
			},
		},
	}

	orch := &Orchestrator{
		Provider:      provider,
		Tools:         tools,
		Permissions:   Ruleset{{Permission: "*", Pattern: "*", Action: ActionAllow}},
		MaxIterations: 5,
	}
	initial := []Message{
		{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "echo round-trip"}}}},
	}

	trail, err := orch.Run(context.Background(), initial)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount != 2 {
		t.Errorf("Stream calls = %d, want 2 (one per iteration)", provider.callCount)
	}
	if len(trail) != 4 {
		t.Fatalf("trail length = %d, want 4 (initial, assistant#1, user-result, assistant#2)", len(trail))
	}

	// trail[1]: assistant with tool_use
	if trail[1].Role != RoleAssistant {
		t.Errorf("trail[1].Role = %q, want assistant", trail[1].Role)
	}
	tuParts := collectToolUses(trail[1].Parts)
	if len(tuParts) != 1 || tuParts[0].Name != "echo" {
		t.Errorf("trail[1] tool_uses = %+v, want one echo", tuParts)
	}

	// trail[2]: user with tool_result
	if trail[2].Role != RoleUser {
		t.Errorf("trail[2].Role = %q, want user", trail[2].Role)
	}
	if len(trail[2].Parts) != 1 || trail[2].Parts[0].Kind != PartToolResult {
		t.Fatalf("trail[2].Parts = %+v, want one tool_result Part", trail[2].Parts)
	}
	tr := trail[2].Parts[0].ToolResult
	if tr.ToolUseID != "tu_1" {
		t.Errorf("tool_result.ToolUseID = %q, want tu_1", tr.ToolUseID)
	}
	if tr.Content != "round-trip" {
		t.Errorf("tool_result.Content = %q, want round-trip", tr.Content)
	}
	if tr.IsError {
		t.Errorf("tool_result.IsError = true, want false on success")
	}

	// trail[3]: final assistant, no tool_use → loop terminates
	if trail[3].Role != RoleAssistant {
		t.Errorf("trail[3].Role = %q, want assistant", trail[3].Role)
	}
	if len(collectToolUses(trail[3].Parts)) != 0 {
		t.Errorf("trail[3] still has tool_uses; loop should have continued")
	}
	if trail[3].StopReason != "end_turn" {
		t.Errorf("trail[3].StopReason = %q, want end_turn", trail[3].StopReason)
	}
}

// TestTwoConsecutiveToolCalls pins multi-iteration behavior. The model
// asks for echo TWICE in successive iterations (iter 1: echo "first";
// iter 2: based on the result, echo "second"; iter 3: end_turn).
// Trail = initial + 3 assistants + 2 user-results = 6 messages. Stream
// called 3 times.
//
// Why this matters: the inter-iteration state (the trail growing by 2
// each loop pass) is the load-bearing thing. If the second iteration's
// Provider.Stream were called with the WRONG Messages slice (e.g.
// stale, or missing the first tool_result), the test wouldn't catch it
// directly — but the iteration count and trail shape ARE the observable
// proxy for "the trail grew correctly between iterations."
func TestTwoConsecutiveToolCalls(t *testing.T) {
	tools := NewRegistry()
	if err := tools.Register(EchoTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	provider := &fakeProvider{
		scripts: [][]Event{
			{
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID: "tu_1", Name: "echo", Input: json.RawMessage(`{"text":"first"}`),
				}},
				{Type: EventFinish},
			},
			{
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID: "tu_2", Name: "echo", Input: json.RawMessage(`{"text":"second"}`),
				}},
				{Type: EventFinish},
			},
			{
				{Type: EventText, Text: "all done"},
				{Type: EventFinish},
			},
		},
	}

	orch := &Orchestrator{
		Provider:      provider,
		Tools:         tools,
		Permissions:   Ruleset{{Permission: "*", Pattern: "*", Action: ActionAllow}},
		MaxIterations: 10,
	}
	initial := []Message{
		{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "do two echoes"}}}},
	}

	trail, err := orch.Run(context.Background(), initial)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount != 3 {
		t.Errorf("Stream calls = %d, want 3", provider.callCount)
	}
	if len(trail) != 6 {
		t.Fatalf("trail length = %d, want 6 (initial + 3 asst + 2 user-result)", len(trail))
	}

	// Verify the order of tool_results: tu_1 in trail[2], tu_2 in trail[4].
	if id := trail[2].Parts[0].ToolResult.ToolUseID; id != "tu_1" {
		t.Errorf("trail[2] ToolUseID = %q, want tu_1", id)
	}
	if got := trail[2].Parts[0].ToolResult.Content; got != "first" {
		t.Errorf("trail[2] content = %q, want first", got)
	}
	if id := trail[4].Parts[0].ToolResult.ToolUseID; id != "tu_2" {
		t.Errorf("trail[4] ToolUseID = %q, want tu_2", id)
	}
	if got := trail[4].Parts[0].ToolResult.Content; got != "second" {
		t.Errorf("trail[4] content = %q, want second", got)
	}
}

// TestPermissionDenyProducesErrorResult pins the deny → tool_result
// translation. The model asks for echo; the ruleset denies "echo:*"; the
// Orchestrator does NOT execute the tool but still returns successfully,
// with the tool_result Part carrying IsError=true and a "permission denied"
// message. The next iteration sees the error and ends turn.
//
// Run returns nil error — denial is a normal in-band signal to the LLM,
// not a Run-level failure. This is the contract the test pins: deny
// SURFACES to the LLM, doesn't break the loop.
func TestPermissionDenyProducesErrorResult(t *testing.T) {
	tools := NewRegistry()
	if err := tools.Register(EchoTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	provider := &fakeProvider{
		scripts: [][]Event{
			{
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID: "tu_1", Name: "echo", Input: json.RawMessage(`{"text":"forbidden"}`),
				}},
				{Type: EventFinish},
			},
			{
				{Type: EventText, Text: "I see I'm not allowed; stopping."},
				{Type: EventFinish},
			},
		},
	}

	orch := &Orchestrator{
		Provider: provider,
		Tools:    tools,
		// Deny the echo tool by name. The pattern matches anything
		// (the Input string), so this is a blanket "echo is denied."
		Permissions:   Ruleset{{Permission: "echo", Pattern: "*", Action: ActionDeny}},
		MaxIterations: 5,
	}
	initial := []Message{
		{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "try echo"}}}},
	}

	trail, err := orch.Run(context.Background(), initial)
	if err != nil {
		t.Fatalf("Run returned non-nil err for deny: %v (deny should be in-band)", err)
	}
	if len(trail) != 4 {
		t.Fatalf("trail length = %d, want 4", len(trail))
	}

	tr := trail[2].Parts[0].ToolResult
	if !tr.IsError {
		t.Errorf("tool_result.IsError = false, want true on permission deny")
	}
	if !strings.Contains(tr.Content, "permission denied") {
		t.Errorf("tool_result.Content = %q, want contains 'permission denied'", tr.Content)
	}
	if tr.ToolUseID != "tu_1" {
		t.Errorf("tool_result.ToolUseID = %q, want tu_1", tr.ToolUseID)
	}
}

// TestMaxIterationsExceeded pins the safety cap. The fake assistant ALWAYS
// asks for the same tool ("infinite loop" simulation); MaxIterations=1
// means after the first iteration's tool_result is appended, the next
// loop pass hits the cap and returns ErrMaxIterationsExceeded.
//
// Trail at error time = initial + assistant#1 + user-result#1 = 3 messages.
// The cap fires BEFORE iteration 2's Provider.Stream call, so callCount=1.
//
// This is the proof that a misbehaving model (or an unbounded loop bug)
// can't burn forever. Production wraps Run in a token-budget loop instead;
// MaxIterations is the curriculum's simple stand-in.
func TestMaxIterationsExceeded(t *testing.T) {
	tools := NewRegistry()
	if err := tools.Register(EchoTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// One script — but the test only allows ONE Stream call before
	// hitting the cap. If the cap is broken, fakeProvider runs out of
	// scripts and returns errOutOfScripts, which would fail the test
	// with a different error than we expect.
	provider := &fakeProvider{
		scripts: [][]Event{
			{
				{Type: EventToolUse, ToolUse: &ToolUseEvent{
					ID: "tu_loop", Name: "echo", Input: json.RawMessage(`{"text":"loop"}`),
				}},
				{Type: EventFinish},
			},
		},
	}

	orch := &Orchestrator{
		Provider:      provider,
		Tools:         tools,
		Permissions:   Ruleset{{Permission: "*", Pattern: "*", Action: ActionAllow}},
		MaxIterations: 1,
	}
	initial := []Message{
		{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "loop forever"}}}},
	}

	trail, err := orch.Run(context.Background(), initial)
	if !errors.Is(err, ErrMaxIterationsExceeded) {
		t.Fatalf("err = %v, want ErrMaxIterationsExceeded", err)
	}
	if provider.callCount != 1 {
		t.Errorf("Stream calls = %d, want 1 (cap fires before iter 2)", provider.callCount)
	}
	if len(trail) != 3 {
		t.Errorf("trail length = %d, want 3 (initial + asst + user-result)", len(trail))
	}
	// Even on cap-exceed, the trail's last entry is the user-results
	// Message — the contract is "you can re-Run with a higher cap and
	// pick up where you left off."
	if trail[len(trail)-1].Role != RoleUser {
		t.Errorf("trail last Role = %q, want user (the synthesized tool_results)", trail[len(trail)-1].Role)
	}
}
