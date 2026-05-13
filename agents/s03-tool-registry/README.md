# s03 — tool-registry

s02 gave us typed Parts to read, including `PartToolUse`. But there was nothing on the consumer side to actually *do* the tool call — no map from `tool_use.name` to runnable code, no JSON schema for the LLM to know what tools exist. s03 fills that gap with a `Tool` interface plus a `Registry` that holds tools, serializes them for the LLM, and dispatches by name.

This is the producer side of the Part union: when a `tool_use` Part arrives off the wire (s06's streaming loop will deliver these), s03's Registry is what answers the question "given `name` + `input`, what string does the next `tool_result` Part carry?"

## Files

- `tool.go` — the `Tool` interface (`Name`, `Description`, `JSONSchema`, `Execute`); the `Registry` struct (`Register`, `Lookup`, `Names`, `ToolSchemas`, `Dispatch`); the `ToolSchema` wire type and the `ErrUnknownTool` sentinel.
- `builtin_echo.go` — `EchoTool`: takes `{text: string}`, returns it back. Smallest dispatch-test fixture possible.
- `builtin_now.go` — `NowTool`: takes optional `{format: string}`, returns `time.Now()` formatted (RFC3339 default). Demonstrates the optional-args path and uses an injectable `nowFn` for testability.
- `main.go` — runnable demo: register the two built-ins, print the LLM-facing schemas JSON, dispatch echo + now, print results.
- `tool_test.go` — 4 tests:
  1. **register + lookup** — round-trip a tool through the Registry; deterministic Names() ordering.
  2. **ToolSchemas() validity** — output is valid JSON, has a row per tool, every row has the three keys Anthropic's `tools` API requires.
  3. **dispatch by name** — both tools execute; clock injection makes the `now` assertion exact.
  4. **unknown tool name** — `Dispatch` returns an error wrapping `ErrUnknownTool`, and the message names both the bad tool and the available set.

## Run

```
go run .                # demo: print schemas, dispatch both built-ins
go test -count=1 ./...  # 4 tests, no network, no clock dependency
```

## What this maps to upstream

| This file              | Upstream file                                                        |
|------------------------|----------------------------------------------------------------------|
| `tool.go` `Tool`       | `packages/opencode/src/tool/tool.ts` `Def` interface (lines 35–45)   |
| `tool.go` `Registry`   | `packages/opencode/src/tool/registry.ts` `Service` (lines 70–260)    |
| `builtin_echo.go`      | (no exact upstream — opencode's smallest tool is `read`, which I/Os) |
| `builtin_now.go`       | (no exact upstream — same reason)                                    |

## Key teaching points

- **Tool is an interface, not a struct.** The LLM only ever sees JSON; the Go side needs a polymorphic `Execute`. An interface with 4 methods is exactly that. Closures-as-values would also work — opencode literally passes `execute: (args, ctx) => ...` — but Go's interface idiom keeps Name/Description/JSONSchema close to Execute on one type.
- **Registry holds nothing but tools.** No permission check, no telemetry, no context propagation. Each of those layers in later: s04 wraps Lookup with a permission gate, s10 wraps Execute with the streaming loop. Keep s03 minimal so each later session adds exactly one concern.
- **`json.RawMessage` for input, not a typed struct.** The Tool interface can't know the input shape of every concrete tool, so we hand the bytes. Each tool unmarshals into its own private `*Input` struct on the way in. Cost: an extra `json.Unmarshal` per dispatch. Benefit: the interface stays one signature for all 14 sessions.
- **`ToolSchemas()` returns `[]map[string]any`, not `[]ToolSchema`.** Looks lazy, but it's deliberate: s05's Provider will splice in OpenAI's `function` envelope (or Bedrock's `toolSpec`) at its layer, and a typed struct would force a round-trip through reflection or a second translator. A bag of `any` is provider-neutral.
- **`ErrUnknownTool` is a sentinel.** `errors.Is(err, ErrUnknownTool)` lets the s10 loop catch the LLM hallucinating a tool name and translate it into a `tool_result{IsError:true}` Part the model can recover from — instead of crashing the whole session.

## What changed vs s02

s02 modeled `PartToolUse{Name, Input}` as data — something we *receive* from the LLM. s03 makes Parts *producible*: a `PartToolUse` can now be looked up in a Registry, dispatched, and turned into a `PartToolResult` for the next turn. The two sessions compose without either knowing about the other — Parts is the wire format, Registry is the dispatcher.

See `docs/zh/s03-tool-registry.md` and `docs/en/s03-tool-registry.md` for the long-form walkthrough plus the upstream excerpt from `packages/opencode/src/tool/registry.ts`.
