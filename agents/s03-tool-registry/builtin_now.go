package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// NowTool returns the current time. It's the second built-in for two reasons:
//
//  1. Demonstrates a tool that takes optional input (omit `format` and you get
//     RFC3339; pass a Go time-format string and you get that). The Tool
//     interface deliberately handles both — `input` is `json.RawMessage`, so
//     `null` / `{}` / a partial object are all legal.
//  2. Tests in tool_test.go use NowTool as the second registered tool — both
//     to verify Names() ordering and to catch any single-tool fast paths in
//     the dispatcher that wouldn't survive a real registry of N tools.
//
// `nowFn` is a package-level indirection so tests can replace `time.Now`
// without monkey-patching. opencode does the same in TypeScript via the
// `@opencode-ai/core/util/clock` injection.
var nowFn = time.Now

type NowTool struct{}

type nowInput struct {
	// Format is a Go time-package layout string ("2006-01-02 15:04:05",
	// time.RFC3339, etc.). When empty / omitted we default to RFC3339,
	// which is what every other LLM-readable timestamp in opencode uses.
	Format string `json:"format,omitempty"`
}

func (NowTool) Name() string { return "now" }

func (NowTool) Description() string {
	return "Return the current time. Pass `format` (Go time-layout string) to override the default RFC3339."
}

func (NowTool) JSONSchema() (string, error) {
	// Note: `required` is empty — both fields are optional. The LLM is
	// allowed to send `{}` or omit `input` entirely. Anthropic's tool
	// schema validator accepts this only when `required` is present (even
	// if empty); we include it explicitly to keep the wire shape stable.
	return `{
		"type": "object",
		"properties": {
			"format": {
				"type": "string",
				"description": "Optional Go time-layout string. Defaults to RFC3339."
			}
		},
		"required": []
	}`, nil
}

func (NowTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args nowInput
	// `input` may legitimately be empty / null when the LLM sends `{}`;
	// allow that explicitly rather than treating it as a parse error.
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("now: parse input: %w", err)
		}
	}
	layout := args.Format
	if layout == "" {
		layout = time.RFC3339
	}
	return nowFn().Format(layout), nil
}
