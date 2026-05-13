package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// EchoTool is the simplest possible Tool: it returns whatever string the LLM
// passed in `text`. We ship it for two reasons:
//
//  1. tool_test.go needs a deterministic Tool to dispatch against — no clocks,
//     no I/O, no env. Echo is that.
//  2. The README's "go run ." demo round-trips the LLM wire format end-to-end
//     without any real model in the loop.
//
// opencode has no exact equivalent — its smallest real tool is `read`, which
// already touches the filesystem. EchoTool is purely a teaching shim.
type EchoTool struct{}

// echoInput is the typed shape of EchoTool's args. Kept private to this file
// so other tools can't accidentally collide on the schema.
type echoInput struct {
	Text string `json:"text"`
}

func (EchoTool) Name() string { return "echo" }

func (EchoTool) Description() string {
	return "Echo the provided text back verbatim. Useful for testing tool dispatch."
}

// JSONSchema is hand-written rather than reflected from echoInput. At s03's
// scale (two tools) the savings from a reflection-based generator don't pay
// for the dependency; we revisit when s12 (MCP) introduces remote tools whose
// schemas come pre-computed from the upstream server.
func (EchoTool) JSONSchema() (string, error) {
	return `{
		"type": "object",
		"properties": {
			"text": {
				"type": "string",
				"description": "The text to echo back."
			}
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
