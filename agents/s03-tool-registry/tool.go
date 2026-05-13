package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Tool is the contract every callable tool must satisfy. It mirrors opencode's
// `Tool.Def` interface in packages/opencode/src/tool/tool.ts — the fields the
// LLM sees (name + description + JSON schema) and the closure the runtime
// calls when it picks that tool from a `tool_use` Part (s02).
//
// Two deliberate Go-side simplifications vs upstream:
//
//  1. opencode validates `args` through Effect's `Schema.decodeUnknownEffect`
//     in `wrap()`; we hand the raw `json.RawMessage` to the tool and let it
//     `json.Unmarshal` into its own typed input struct. Less machinery,
//     identical contract: bad input → tool returns error → loop surfaces it
//     to the LLM as a tool_result with is_error=true.
//
//  2. opencode returns `ExecuteResult{title, output, metadata, attachments}`;
//     s03 returns just the `output` string. `title`/`metadata` are UI signals
//     and `attachments` is for file results — both land in s07 (persistence)
//     and s10 (loop), not here.
type Tool interface {
	// Name is the stable, snake_case identifier the LLM uses in `tool_use.name`.
	// Must match the JSON schema's slot in Registry.ToolSchemas() output.
	Name() string

	// Description is human-prose shown to the LLM as part of its tool list.
	// Anthropic's models read this verbatim when deciding which tool to call —
	// keep it short, action-oriented, no quoting.
	Description() string

	// JSONSchema returns the args object schema as a JSON Schema (draft-07
	// shape: `{"type":"object","properties":{...},"required":[...]}`).
	// Returned as a string so the implementation can either hand-write the
	// schema literal (cheap, what we do here) or build it from struct tags
	// (more work, deferred to s05's Provider abstraction).
	JSONSchema() (string, error)

	// Execute runs the tool with the LLM-provided JSON args and returns the
	// string the LLM will see in the next turn's tool_result Part. Errors
	// surface as ToolResult{IsError:true} once we wire up s10's loop.
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// ToolSchema is the wire shape the LLM sees in the API request's `tools` field.
// Same flat object Anthropic's API consumes:
//
//	{"name":"echo", "description":"...", "input_schema":{...JSON schema...}}
//
// opencode calls this `Tool.Def` (TypeScript) and projects to OpenAI/Anthropic
// shapes via the AI SDK; we keep one Go shape and let s05's Provider translate
// for non-Anthropic backends.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Registry is the in-process map of (name → Tool). Holds nothing but tools —
// permission, context propagation, telemetry all layer on top in later
// sessions (s04 wraps Lookup with a Permission.evaluate call; s10 wraps
// Execute with the streaming loop).
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty Registry. Tools are added via Register; lookups
// via Lookup. Concurrent reads are safe once construction is done — but we
// don't lock, because the expected lifecycle is "register at startup, lookup
// per request" (mirrors opencode's `InstanceState` pattern).
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a Tool keyed by its Name(). Last-write-wins semantics — same
// as opencode's plugin loader, which lets later plugins shadow earlier ones
// of the same id. Returns an error only if Name() is empty (an LLM-side bug
// we'd rather catch at register time than at dispatch time).
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return fmt.Errorf("tool: empty Name() — LLM cannot dispatch unnamed tools")
	}
	r.tools[name] = t
	return nil
}

// Lookup returns the Tool registered under `name`, plus a found-bool. The
// bool form (rather than `(Tool, error)`) matches Go's "comma ok" idiom and
// lets the caller pick its own error path — opencode's processor returns a
// synthetic ToolResult{IsError:true, Content:"unknown tool"} when this fails.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns every registered tool name in deterministic (sorted) order.
// Useful for logging and for the s10 loop's "what tools is this agent
// allowed to see" filter (which intersects with permission rules per Agent).
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolSchemas serializes every registered Tool into the wire shape the LLM
// API consumes in its `tools` field. Order is deterministic (sorted by name)
// so that two registries built from the same tool set produce byte-identical
// requests — handy for cache keys and snapshot tests.
//
// Returned as `[]map[string]any` (rather than `[]ToolSchema`) so callers can
// splice in provider-specific fields (e.g. OpenAI's `function` envelope) at
// their layer. s05's Provider does that translation; here we stay neutral.
func (r *Registry) ToolSchemas() ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(r.tools))
	for _, name := range r.Names() {
		t := r.tools[name]
		schemaStr, err := t.JSONSchema()
		if err != nil {
			return nil, fmt.Errorf("tool %q: build JSONSchema: %w", name, err)
		}
		var schema any
		if err := json.Unmarshal([]byte(schemaStr), &schema); err != nil {
			return nil, fmt.Errorf("tool %q: JSONSchema is not valid JSON: %w", name, err)
		}
		out = append(out, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": schema,
		})
	}
	return out, nil
}

// Dispatch looks up `name` and runs the tool with `input`. Returns:
//   - the tool's output on success,
//   - a wrapped error if the tool exists but Execute fails,
//   - ErrUnknownTool wrapped if the name isn't registered.
//
// In s10 this becomes the inner step of the streaming loop: an LLM `tool_use`
// Part arrives → permission check → Dispatch → emit a `tool_result` Part.
func (r *Registry) Dispatch(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.Lookup(name)
	if !ok {
		return "", fmt.Errorf("dispatch: %w: %q (registered: %v)", ErrUnknownTool, name, r.Names())
	}
	out, err := t.Execute(ctx, input)
	if err != nil {
		return "", fmt.Errorf("dispatch %q: %w", name, err)
	}
	return out, nil
}

// ErrUnknownTool is returned by Dispatch when the LLM names a tool we don't
// have. Sentinel so callers can `errors.Is(err, ErrUnknownTool)` in s10's
// loop and translate to a `tool_result{IsError:true}` Part instead of crashing.
var ErrUnknownTool = fmt.Errorf("unknown tool")
