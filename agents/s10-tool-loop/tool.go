package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Tool — re-implemented from s03 verbatim. The Orchestrator below treats
// any registered Tool the same way: name lookup, then Execute(ctx, input).
// We don't carry s03's JSONSchema-as-string + ToolSchemas helpers here
// because s10's tests don't need to serialize the schema list to a real
// LLM API — the fake Provider doesn't validate Request.Tools. The hooks
// remain in the interface so a future caller (a real Anthropic provider
// in s11+) could call them without re-shaping Tool.
type Tool interface {
	Name() string
	Description() string
	JSONSchema() (string, error)
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// Registry is the in-process map of (name → Tool). Holds nothing but tools;
// permission, context propagation, telemetry all layer on top in s10's
// Orchestrator. Same shape as s03's Registry; reproduced to keep this
// module self-contained.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty Registry. Tools are added via Register;
// lookups via Lookup. Concurrent reads are safe once construction is done
// — same lifecycle assumption as s03 ("register at startup, lookup per
// request").
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a Tool keyed by its Name(). Last-write-wins semantics, same
// as s03. Returns an error only if Name() is empty.
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return fmt.Errorf("tool: empty Name() — Orchestrator cannot dispatch unnamed tools")
	}
	r.tools[name] = t
	return nil
}

// Lookup returns the Tool registered under `name`, plus a found-bool. The
// Orchestrator uses the bool to synthesize a tool_result{IsError:true,
// Content:"unknown tool: ..."} when an LLM hallucinates a tool name —
// rather than crashing, it lets the model recover next iteration.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns every registered tool name in deterministic (sorted) order.
// Used by the demo's stdout and by error messages that list "tools you
// could've called instead."
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
