package main

import (
	"context"
	"encoding/json"
	"errors"
)

// DieTool — a Tool that always returns an error. Used by the test that
// pins "tool returned an error" → the Orchestrator emits a
// tool_result{IsError:true, Content:<error msg>} Part instead of bubbling
// the error up. The LLM sees the error, can recover, and the loop
// continues until it stops asking for tools (or hits MaxIterations).
//
// We don't have an opencode equivalent of this — every real tool either
// succeeds or returns a typed error that the wrap layer flattens. DieTool
// is a teaching shim that proves the error → tool_result translation works
// in isolation, before s14 builds richer error classification on top.
type DieTool struct{}

func (DieTool) Name() string { return "die" }

func (DieTool) Description() string {
	return "Always fails. Used to test the Orchestrator's tool-error → tool_result conversion."
}

func (DieTool) JSONSchema() (string, error) {
	return `{"type": "object", "properties": {}}`, nil
}

func (DieTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "", errors.New("die: this tool always fails")
}
