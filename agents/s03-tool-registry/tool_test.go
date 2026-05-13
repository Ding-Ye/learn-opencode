package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRegisterAndLookup is the smallest integrity check on the Registry: a
// registered tool comes back out under its Name(). If this fails, the map
// indexing is wrong and every other test is meaningless — so we assert it
// first as a bulkhead.
func TestRegisterAndLookup(t *testing.T) {
	reg := NewRegistry()

	if err := reg.Register(EchoTool{}); err != nil {
		t.Fatalf("Register(echo): %v", err)
	}
	if err := reg.Register(NowTool{}); err != nil {
		t.Fatalf("Register(now): %v", err)
	}

	got, ok := reg.Lookup("echo")
	if !ok {
		t.Fatal("Lookup(echo): not found")
	}
	if got.Name() != "echo" {
		t.Errorf("Lookup(echo).Name() = %q, want echo", got.Name())
	}

	// Names() is deterministic-sorted. Two tools, alphabetic order: echo, now.
	names := reg.Names()
	if len(names) != 2 || names[0] != "echo" || names[1] != "now" {
		t.Errorf("Names() = %v, want [echo now]", names)
	}

	// Lookup of an unknown name returns ok=false (not an error). This matches
	// Go's comma-ok idiom and lets s10's loop translate to a tool_result{IsError}.
	if _, ok := reg.Lookup("nope"); ok {
		t.Errorf("Lookup(nope) returned ok=true, want false")
	}
}

// TestToolSchemasIsValidJSON exercises the LLM-facing wire output of the
// Registry. The output must be valid JSON, must contain a row per registered
// tool, and each row must have the three keys Anthropic's `tools` field
// requires (name / description / input_schema).
func TestToolSchemasIsValidJSON(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(EchoTool{})
	_ = reg.Register(NowTool{})

	schemas, err := reg.ToolSchemas()
	if err != nil {
		t.Fatalf("ToolSchemas: %v", err)
	}
	if len(schemas) != 2 {
		t.Fatalf("len(schemas) = %d, want 2", len(schemas))
	}

	// Marshal back to JSON to confirm the whole structure is JSON-clean.
	wire, err := json.Marshal(schemas)
	if err != nil {
		t.Fatalf("marshal schemas: %v", err)
	}

	// Every tool row must have the 3 required keys.
	for i, row := range schemas {
		for _, k := range []string{"name", "description", "input_schema"} {
			if _, present := row[k]; !present {
				t.Errorf("schemas[%d] missing %q key (got keys %v)", i, k, mapKeys(row))
			}
		}
		// input_schema must be an object with type:object — that's how
		// Anthropic's API recognizes a tool args schema.
		schema, ok := row["input_schema"].(map[string]any)
		if !ok {
			t.Errorf("schemas[%d].input_schema is not an object: %T", i, row["input_schema"])
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("schemas[%d].input_schema.type = %v, want object", i, schema["type"])
		}
	}

	// Wire JSON must mention both tool names — sanity check that the
	// serialization didn't silently drop a tool.
	if !strings.Contains(string(wire), `"echo"`) || !strings.Contains(string(wire), `"now"`) {
		t.Errorf("wire missing a tool name: %s", string(wire))
	}
}

// TestDispatchByName drives the inner contract of s10's future loop: given a
// tool name and JSON input, Dispatch must call the tool's Execute and return
// its output verbatim. We use NowTool with an injected clock so the assertion
// is exact rather than time-dependent.
func TestDispatchByName(t *testing.T) {
	// Freeze time so the assertion is exact. Restored via defer so other
	// tests in the package keep the real clock.
	frozen := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return frozen }
	defer func() { nowFn = prevNow }()

	reg := NewRegistry()
	_ = reg.Register(EchoTool{})
	_ = reg.Register(NowTool{})

	// echo round-trip
	got, err := reg.Dispatch(context.Background(), "echo", json.RawMessage(`{"text":"hello s03"}`))
	if err != nil {
		t.Fatalf("Dispatch(echo): %v", err)
	}
	if got != "hello s03" {
		t.Errorf("Dispatch(echo) = %q, want %q", got, "hello s03")
	}

	// now with no args → RFC3339 of the frozen clock
	got, err = reg.Dispatch(context.Background(), "now", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Dispatch(now empty): %v", err)
	}
	if want := frozen.Format(time.RFC3339); got != want {
		t.Errorf("Dispatch(now) = %q, want %q", got, want)
	}

	// now with custom format → Go layout applied to the frozen clock
	got, err = reg.Dispatch(context.Background(), "now", json.RawMessage(`{"format":"2006-01-02"}`))
	if err != nil {
		t.Fatalf("Dispatch(now formatted): %v", err)
	}
	if got != "2026-01-15" {
		t.Errorf("Dispatch(now formatted) = %q, want 2026-01-15", got)
	}
}

// TestDispatchUnknownToolReturnsError pins down the failure mode the s10 loop
// will rely on: if the LLM hallucinates a tool name we don't have, we return
// errors.Is(err, ErrUnknownTool) so the loop can translate to a synthetic
// tool_result{IsError:true} instead of panicking.
func TestDispatchUnknownToolReturnsError(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(EchoTool{})

	_, err := reg.Dispatch(context.Background(), "fly_to_mars", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Dispatch(unknown): want error, got nil")
	}
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("Dispatch(unknown) error not wrapping ErrUnknownTool: %v", err)
	}
	// Error message must mention the bad name AND the registered set, so an
	// operator skimming logs can fix the agent's tool list quickly.
	msg := err.Error()
	if !strings.Contains(msg, "fly_to_mars") {
		t.Errorf("error missing bad name: %q", msg)
	}
	if !strings.Contains(msg, "echo") {
		t.Errorf("error missing registered names hint: %q", msg)
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
