package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// EchoTool — the simplest possible Tool: returns its `text` input verbatim.
// Used by every "happy path" test in loop_test.go because it has no side
// effects and a deterministic output. Mirrors s03's EchoTool exactly.
type EchoTool struct{}

type echoInput struct {
	Text string `json:"text"`
}

func (EchoTool) Name() string { return "echo" }

func (EchoTool) Description() string {
	return "Echo the provided text back verbatim."
}

func (EchoTool) JSONSchema() (string, error) {
	return `{
		"type": "object",
		"properties": {
			"text": {"type": "string"}
		},
		"required": ["text"]
	}`, nil
}

func (EchoTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args echoInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("echo: parse input: %w", err)
	}
	if args.Text == "" {
		return "", fmt.Errorf("echo: 'text' is required")
	}
	return args.Text, nil
}
