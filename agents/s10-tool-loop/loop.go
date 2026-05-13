package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Orchestrator is the THING — the integration that ties Provider +
// Registry + Permission + Message together into the tool execution loop.
// Mirrors `Handle.process` in opencode's
// `packages/opencode/src/session/processor.ts` L734-L802 — same shape
// (per-iteration: stream → tool dispatch → maybe-continue), the same
// terminating conditions (no tool calls → "stop"; needsCompaction → halt),
// just expressed as a plain Go for-loop instead of an Effect runtime
// pipeline.
//
// What it does NOT do (deliberate scope cuts vs upstream):
//   - persistence (s07's job — Orchestrator hands you the final []Message,
//     the caller decides what to write to SQLite)
//   - retry / classification (s14's job — a transport error here aborts
//     immediately; production wraps Run in a retry loop)
//   - compaction (s14's job — opencode's processor flips a "needsCompaction"
//     bit when usage exceeds the model's context window; we don't track usage)
//   - doom-loop detection (upstream's `DOOM_LOOP_THRESHOLD = 3` —
//     "the model called the same tool 3 times with the same args; ask
//     the user if they want to continue"; that's behavior s14's retry
//     classification will own)
//
// What it DOES do — the load-bearing 5-step iteration:
//
//  1. Build a Request from the running []Message.
//  2. Provider.Stream(ctx, req) → drain Events into ONE assistant Message.
//  3. If that Message contains no tool_use Parts → done, return.
//  4. For each tool_use Part:
//     a. Permission.Evaluate(name, target, rules) — Deny → synthesize
//        a tool_result{IsError:true} Part instead of running the tool.
//     b. Allow / Ask → Registry.Lookup + Tool.Execute, capture (output,
//        err). err → tool_result{IsError:true, Content: err}; output →
//        tool_result{IsError:false, Content: output}.
//  5. Append a USER Message containing all the tool_result Parts.
//
// Repeat until step 3 fires OR MaxIterations is hit.
type Orchestrator struct {
	// Provider is the LLM. Required.
	Provider Provider

	// Tools is the runtime tool registry. May be empty — a Provider that
	// never asks for a tool (e.g. a pure text response) will still run
	// to completion against an empty Registry.
	Tools *Registry

	// Permissions is the FLAT, already-merged ruleset the s09 cascade
	// produces (defaults ++ userConfig ++ agentOverride). Evaluate walks
	// this slice last-match-wins. nil ⇒ ActionAsk for every tool, which
	// in s10's headless contract collapses to "allow" (Ask defaults to
	// Allow because there's no human to ask). A real interactive build
	// would replace AskHandler with a prompt; we don't have one.
	Permissions Ruleset

	// MaxIterations is the cap on Provider.Stream calls per Run. 0 means
	// "unlimited" — but tests always pin a small number so a buggy loop
	// can't spin forever. Production opencode doesn't cap iterations
	// directly; it gates on cost / token budget instead (s14's job).
	// In the curriculum, MaxIterations is the simplest demonstration of
	// "the loop is bounded."
	MaxIterations int
}

// ErrMaxIterationsExceeded is returned by Run when the assistant keeps
// asking for tools and the iteration count hits MaxIterations. Sentinel so
// callers can `errors.Is(err, ErrMaxIterationsExceeded)` and pivot to a
// "Tell the user we hit the cap" UI.
var ErrMaxIterationsExceeded = errors.New("orchestrator: max iterations exceeded")

// Run executes the tool loop. Takes the initial []Message (typically one
// user Message; could be a multi-turn history for a resumed session) and
// returns the FULL trail at the end — initial + every assistant Message
// produced + every synthesized tool-result user Message. The trail's
// last entry is always either:
//   - an assistant Message with NO tool_use Parts (natural end_turn), OR
//   - a user Message of tool_results immediately followed by — wait, no:
//     if Run hits MaxIterations after appending the user-tool-results
//     Message, the trail ends with that user Message; the error pins the
//     reason. This is intentional: the caller can re-Run with a higher cap
//     and the trail picks up exactly where it left off.
func (o *Orchestrator) Run(ctx context.Context, initial []Message) ([]Message, error) {
	if o == nil || o.Provider == nil {
		return nil, errors.New("orchestrator: nil Provider")
	}
	if o.Tools == nil {
		// Empty registry is fine — Lookup will miss every name and the
		// Orchestrator will surface "unknown tool" tool_results. Saves
		// the test boilerplate of building a Registry just to NOT
		// register anything.
		o.Tools = NewRegistry()
	}

	// Trail accumulates every Message in the order they happened.
	// Initial messages are copied so the caller's slice isn't aliased
	// — they may want to keep their original "user said X" array intact.
	trail := make([]Message, 0, len(initial)+4)
	trail = append(trail, initial...)

	for iter := 0; ; iter++ {
		if o.MaxIterations > 0 && iter >= o.MaxIterations {
			return trail, ErrMaxIterationsExceeded
		}
		if err := ctx.Err(); err != nil {
			return trail, err
		}

		// Build the Request from the running trail. Translation runs
		// every iteration — the trail grew by 2 since last time (one
		// assistant + one user-tool-results), and the LLM needs the
		// updated context.
		req := Request{
			Messages: messagesToProvider(trail),
			Tools:    toolSchemas(o.Tools),
		}

		assistant, err := o.consumeOne(ctx, req)
		if err != nil {
			return trail, err
		}
		trail = append(trail, *assistant)

		// Find tool_use Parts. If none, the assistant ended its turn
		// with text/reasoning only — that's natural termination.
		toolUses := collectToolUses(assistant.Parts)
		if len(toolUses) == 0 {
			return trail, nil
		}

		// Build the user Message of tool_results. ONE Message with one
		// tool_result Part per tool_use, in arrival order. This matches
		// Anthropic's API contract: all tool_results for a given
		// assistant turn go in the SAME user message.
		userMsg := Message{Role: RoleUser, Parts: make([]Part, 0, len(toolUses))}
		for _, tu := range toolUses {
			result := o.runOneTool(ctx, tu)
			userMsg.Parts = append(userMsg.Parts, Part{
				Kind:       PartToolResult,
				ToolResult: result,
			})
		}
		trail = append(trail, userMsg)

		// Loop continues — next iteration's Stream call will see the
		// updated trail (initial + assistant + userMsg + ...).
	}
}

// runOneTool is the per-tool-use inner step. Three outcomes, all flattened
// to a *ToolResultPart so the caller (Run above) can append uniformly:
//
//  1. Permission Deny → IsError=true, Content="permission denied: ...".
//  2. Tool not found  → IsError=true, Content="unknown tool: ...".
//  3. Tool err       → IsError=true, Content=<error string>.
//  4. Tool ok        → IsError=false, Content=<output string>.
//
// Permission "Ask" is treated as Allow — the headless contract. Real
// interactive opencode would `permission.ask({...})` here and wait on the
// user's reply (see processor.ts L386-L394).
func (o *Orchestrator) runOneTool(ctx context.Context, tu *ToolUsePart) *ToolResultPart {
	// Permission gate. The "target" we evaluate against is the tool's
	// stringified input — that's what the s04 wildcard matcher expects
	// for things like `bash:rm -rf*`. For `edit:*.go` it expects a path,
	// which the caller would extract from the input JSON; we pass the
	// raw input verbatim so any pattern that matches the JSON string
	// representation works. Production builds parse out specific fields
	// (path / cmd / url) per permission domain; s10 keeps the raw form
	// so the test suite can express simple "name-only" rules.
	target := string(tu.Input)
	action := Evaluate(tu.Name, target, o.Permissions)
	if action == ActionDeny {
		return &ToolResultPart{
			ToolUseID: tu.ID,
			Content:   fmt.Sprintf("permission denied: %s", tu.Name),
			IsError:   true,
		}
	}
	// ActionAllow and ActionAsk both fall through to execution — see the
	// docstring above for why "Ask" defaults to "Allow" in the headless
	// contract.

	tool, ok := o.Tools.Lookup(tu.Name)
	if !ok {
		return &ToolResultPart{
			ToolUseID: tu.ID,
			Content:   fmt.Sprintf("unknown tool: %s (available: %v)", tu.Name, o.Tools.Names()),
			IsError:   true,
		}
	}

	out, err := tool.Execute(ctx, tu.Input)
	if err != nil {
		return &ToolResultPart{
			ToolUseID: tu.ID,
			Content:   err.Error(),
			IsError:   true,
		}
	}
	return &ToolResultPart{
		ToolUseID: tu.ID,
		Content:   out,
		IsError:   false,
	}
}

// consumeOne drains exactly ONE Provider.Stream call into ONE assistant
// Message. Same assembly rules as s06's Loop.Consume — adjacent text
// deltas collapse into one PartText, tool_use breaks the run, reasoning
// deltas collapse the same way as text. Reproduced here (rather than
// imported from s06) because each session is its own module.
func (o *Orchestrator) consumeOne(ctx context.Context, req Request) (*Message, error) {
	stream, err := o.Provider.Stream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provider.Stream: %w", err)
	}
	defer stream.Close()

	msg := &Message{Role: RoleAssistant}
	trailing := PartUnknown

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			if msg.StopReason == "" {
				msg.StopReason = inferStopReason(msg.Parts)
			}
			return msg, nil
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, fmt.Errorf("stream.Next: %w", err)
		}

		switch ev.Type {
		case EventText:
			if trailing == PartText && len(msg.Parts) > 0 {
				last := &msg.Parts[len(msg.Parts)-1]
				if last.Text == nil {
					last.Text = &TextPart{}
				}
				last.Text.Text += ev.Text
			} else {
				msg.Parts = append(msg.Parts, Part{
					Kind: PartText,
					Text: &TextPart{Text: ev.Text},
				})
			}
			trailing = PartText

		case EventToolUse:
			if ev.ToolUse == nil {
				return nil, errors.New("orchestrator: EventToolUse with nil ToolUse")
			}
			if ev.ToolUse.Name == "" {
				return nil, fmt.Errorf("orchestrator: EventToolUse missing tool name (id=%q)", ev.ToolUse.ID)
			}
			msg.Parts = append(msg.Parts, Part{
				Kind: PartToolUse,
				ToolUse: &ToolUsePart{
					ID:    ev.ToolUse.ID,
					Name:  ev.ToolUse.Name,
					Input: ev.ToolUse.Input,
				},
			})
			trailing = PartToolUse

		case EventReasoning:
			if trailing == PartReasoning && len(msg.Parts) > 0 {
				last := &msg.Parts[len(msg.Parts)-1]
				if last.Reasoning == nil {
					last.Reasoning = &ReasoningPart{}
				}
				last.Reasoning.Text += ev.Reasoning
			} else {
				msg.Parts = append(msg.Parts, Part{
					Kind:      PartReasoning,
					Reasoning: &ReasoningPart{Text: ev.Reasoning},
				})
			}
			trailing = PartReasoning

		case EventFinish:
			if ev.Usage != nil {
				usageCopy := *ev.Usage
				msg.Usage = &usageCopy
			}
			if msg.StopReason == "" {
				msg.StopReason = inferStopReason(msg.Parts)
			}

		default:
			// Unknown event type — opencode logs and ignores; same here.
		}
	}
}

// collectToolUses walks the assembled assistant Message and returns
// pointers to every PartToolUse, in order. The Orchestrator then runs
// each tool sequentially. Sequential, not parallel, because:
//   - opencode runs them sequentially too (each tool's snapshot/permission
//     handshake is naturally serial in processor.ts);
//   - parallel execution would require per-tool context cancellation
//     plumbing we don't have at s10's scope.
//
// A future ext-exercise could turn this into errgroup.Wait + a per-tool
// goroutine; the Orchestrator's contract wouldn't change.
func collectToolUses(parts []Part) []*ToolUsePart {
	var out []*ToolUsePart
	for i := range parts {
		if parts[i].Kind == PartToolUse && parts[i].ToolUse != nil {
			out = append(out, parts[i].ToolUse)
		}
	}
	return out
}

// inferStopReason picks "tool_use" if the assembled message ended with a
// tool_use Part, "end_turn" otherwise. Same heuristic as s06.
func inferStopReason(parts []Part) string {
	if len(parts) == 0 {
		return "end_turn"
	}
	if parts[len(parts)-1].Kind == PartToolUse {
		return "tool_use"
	}
	return "end_turn"
}

// messagesToProvider translates the in-memory []Message (Parts-based,
// the Orchestrator's working format) into []ProviderMessage (ContentBlocks-
// based, the wire format the Provider speaks). The two shapes are
// near-isomorphic; this is the boundary translator.
//
// One subtlety: assistant Messages emit text + tool_use ContentBlocks;
// user Messages emit text + tool_result ContentBlocks. We don't try to
// be clever about which Parts are valid for which Role — we just project
// every Part to its matching ContentBlock. A bug in upstream message
// construction (e.g. an assistant message with a tool_result Part) would
// produce a malformed Request, but the test suite would catch it as a
// fakeProvider script that doesn't match expectations.
func messagesToProvider(msgs []Message) []ProviderMessage {
	out := make([]ProviderMessage, 0, len(msgs))
	for _, m := range msgs {
		pm := ProviderMessage{Role: string(m.Role)}
		for _, p := range m.Parts {
			switch p.Kind {
			case PartText:
				if p.Text != nil {
					pm.Content = append(pm.Content, ContentBlock{
						Type: "text",
						Text: p.Text.Text,
					})
				}
			case PartToolUse:
				if p.ToolUse != nil {
					pm.Content = append(pm.Content, ContentBlock{
						Type:  "tool_use",
						ID:    p.ToolUse.ID,
						Name:  p.ToolUse.Name,
						Input: p.ToolUse.Input,
					})
				}
			case PartToolResult:
				if p.ToolResult != nil {
					pm.Content = append(pm.Content, ContentBlock{
						Type:      "tool_result",
						ToolUseID: p.ToolResult.ToolUseID,
						Content:   p.ToolResult.Content,
						IsError:   p.ToolResult.IsError,
					})
				}
			case PartReasoning:
				// Reasoning parts don't go back to the LLM in the next
				// turn — they're for the user/persistence. opencode's
				// processor strips them similarly when re-encoding the
				// request (see processor.ts L744 — the LLM service
				// reads from session messages, which expose reasoning
				// only in the v2 events stream, not in the prompt).
			}
		}
		out = append(out, pm)
	}
	return out
}

// toolSchemas projects the Registry into the wire-shape Request.Tools
// expects. The fake Provider doesn't use this (it ignores Request), but
// emitting a non-empty Tools slice keeps the Request close to what a
// real Anthropic client would receive — useful when the demo is wired
// up to a real provider in s11+.
func toolSchemas(r *Registry) []ToolSchema {
	if r == nil {
		return nil
	}
	out := make([]ToolSchema, 0, len(r.tools))
	for _, name := range r.Names() {
		t := r.tools[name]
		schemaStr, err := t.JSONSchema()
		if err != nil {
			continue
		}
		out = append(out, ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(schemaStr),
		})
	}
	return out
}
