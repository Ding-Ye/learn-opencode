---
title: "s03 · Tool registry"
chapter: 3
slug: s03-tool-registry
est_read_min: 10
---

# s03 · Tool registry

> What this teaches: the producer side of the Part union. s02 modeled `PartToolUse` as data we *receive*. s03 builds the registry that *answers* it — given a tool name and JSON args, run code and return the string the next `tool_result` Part will carry. Get this interface right and every later session (permission gates, streaming dispatch, MCP, LSP) just slots into it.

---

## Problem

s02 ended with a typed `PartToolUse{ID, Name, Input}` arriving off the wire. So… what runs it?

A real agent ships a dozen-plus tools: `read`, `write`, `edit`, `grep`, `shell`, `glob`, `webfetch`, `task`, `todo`, `skill`, `lsp`, ... opencode's `packages/opencode/src/tool/registry.ts` is a 430-line file because it wires per-model filtering, plugin loading, telemetry, and DI. But underneath all that there's exactly one job:

> Given a `tool_use.name`, return a callable that takes the LLM's JSON args and produces a string for the next `tool_result`.

If we don't crystallize that interface now — before s10 introduces the streaming dispatch loop, before s12 introduces remote MCP tools, before s04 introduces permission gates — every later session has to invent its own ad-hoc map and we'll never compose them cleanly.

## Solution

Three pieces, ~120 LOC of Go:

1. **`Tool` interface** with four methods: `Name()`, `Description()`, `JSONSchema()`, `Execute(ctx, json.RawMessage) (string, error)`. The first three are what the LLM sees in its tool list; the fourth is the closure the loop calls when the LLM picks this tool.
2. **`Registry` struct** holding `map[string]Tool`. `Register` adds; `Lookup` reads (comma-ok); `Names` returns sorted keys; `ToolSchemas()` projects to the LLM-facing wire shape; `Dispatch` is the lookup-then-execute one-liner.
3. **Two built-in tools** for tests: `EchoTool` (returns its input) and `NowTool` (returns time.Now, optionally formatted). Tiny and deterministic — exactly what `tool_test.go` needs.

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│  s03 Tool registry                                              │
│                                                                 │
│   On startup:                                                   │
│     reg := NewRegistry()                                        │
│     reg.Register(EchoTool{})  ─┐                                │
│     reg.Register(NowTool{})   ─┴─►  reg.tools = {echo:_, now:_} │
│                                                                 │
│   Sending tools to the LLM (s05's Provider will splice this):   │
│     reg.ToolSchemas() ─► [{name, description, input_schema},..] │
│                                                                 │
│   Receiving a tool_use Part from the LLM (s10's loop):          │
│     PartToolUse{Name:"echo", Input:{"text":"hi"}}               │
│            │                                                    │
│            ▼                                                    │
│     reg.Dispatch(ctx, "echo", input)                            │
│            │                                                    │
│            ├─► Lookup("echo") → EchoTool{}                      │
│            └─► EchoTool.Execute(ctx, input)                     │
│                                                                 │
│            ► returns "hi"                                       │
│                  │                                              │
│                  ▼                                              │
│     PartToolResult{ToolUseID:"...", Content:"hi", IsError:false}│
└─────────────────────────────────────────────────────────────────┘
```

The interface in `tool.go`:

```go
type Tool interface {
    Name() string
    Description() string
    JSONSchema() (string, error)
    Execute(ctx context.Context, input json.RawMessage) (string, error)
}

type Registry struct {
    tools map[string]Tool
}

func (r *Registry) Dispatch(ctx context.Context, name string, input json.RawMessage) (string, error) {
    t, ok := r.Lookup(name)
    if !ok {
        return "", fmt.Errorf("dispatch: %w: %q (registered: %v)", ErrUnknownTool, name, r.Names())
    }
    return t.Execute(ctx, input)  // (with error wrap)
}
```

**Three non-obvious points**:

1. **`json.RawMessage` for input, not a typed struct.** The Tool interface can't know each concrete tool's input shape. Each tool unmarshals into its own private input struct on the way in. Cost: one extra `json.Unmarshal` per dispatch. Benefit: the interface stays one signature for all 14 sessions, and adding a new tool never touches Registry.
2. **`ToolSchemas()` returns `[]map[string]any`, not `[]ToolSchema`.** Looks lazy; it's deliberate. s05's Provider will splice OpenAI's `function` envelope (or Bedrock's `toolSpec`) at its own layer, and a typed Go struct would force a round-trip through reflection or a second translator. A bag of `any` is provider-neutral.
3. **`ErrUnknownTool` is a sentinel.** `errors.Is(err, ErrUnknownTool)` lets s10's loop catch the case where the LLM hallucinates a tool name and translate it into a synthetic `tool_result{IsError:true}` Part — instead of crashing. The error message also names both the bad tool and the registered set, so an operator skimming logs sees the fix immediately.

## What Changed (vs. s02)

s02 added the `Part` union. The `PartToolUse` arm carried `{Name, Input}` but had nothing on the consumer side that knew what to *do* with that name. s03 fills the gap:

```diff
 // s02: Parts can be received and read.
 type Part struct {
     Kind    PartKind
     ToolUse *ToolUseRef  // {ID, Name, Input}
     ...
 }

+// s03: Parts can now be produced. Given a PartToolUse, the Registry
+// answers what string the next PartToolResult should carry.
+type Tool interface {
+    Name() string
+    Description() string
+    JSONSchema() (string, error)
+    Execute(ctx context.Context, input json.RawMessage) (string, error)
+}
+
+type Registry struct{ tools map[string]Tool }
+func (r *Registry) Register(t Tool) error
+func (r *Registry) Lookup(name string) (Tool, bool)
+func (r *Registry) ToolSchemas() ([]map[string]any, error)
+func (r *Registry) Dispatch(ctx context.Context, name string, input json.RawMessage) (string, error)
```

s02's wire-format work is unchanged. s03 only adds — it never re-reads or modifies a Part. That clean producer/consumer split is what lets s10 (the streaming loop) compose the two without either knowing about the other.

## Try It

```bash
cd agents/s03-tool-registry

# Demo: register 2 built-ins, print LLM-facing schemas, dispatch both.
go run .

# 4 tests, no network, no real clock (NowTool uses an injectable nowFn).
go test -count=1 ./...

# Show just the JSON schemas the LLM sees:
go run . | sed -n '/tool_schemas/,/dispatch/p' | head -40
```

## Upstream Source Reading

The mechanism this s03 mirrors lives in opencode's `packages/opencode/src/tool/registry.ts` (the Service definition) plus `packages/opencode/src/tool/tool.ts` (the Def interface). What follows is the State + Interface + Service header of registry.ts, which together are the upstream's "what does a registry hold" answer — every tool the LLM sees comes through this Service's `all()` / `tools()` methods.

```ts
// upstream:packages/opencode/src/tool/registry.ts#L60-L130
type TaskDef = Tool.InferDef<typeof TaskTool>
type ReadDef = Tool.InferDef<typeof ReadTool>

type State = {
  custom: Tool.Def[]
  builtin: Tool.Def[]
  task: TaskDef
  read: ReadDef
}

export interface Interface {
  readonly ids: () => Effect.Effect<string[]>
  readonly all: () => Effect.Effect<Tool.Def[]>
  readonly named: () => Effect.Effect<{ task: TaskDef; read: ReadDef }>
  readonly tools: (model: { providerID: ProviderID; modelID: ModelID; agent: Agent.Info }) => Effect.Effect<Tool.Def[]>
}

export class Service extends Context.Service<Service, Interface>()("@opencode/ToolRegistry") {}

export const layer: Layer.Layer<
  Service,
  never,
  | Config.Service
  | Plugin.Service
  | Question.Service
  | Todo.Service
  | Agent.Service
  | Skill.Service
  | Session.Service
  | Provider.Service
  | Git.Service
  | Reference.Service
  | LSP.Service
  | Instruction.Service
  | AppFileSystem.Service
  | Bus.Service
  | HttpClient.HttpClient
  | ChildProcessSpawner
  | Ripgrep.Service
  | Format.Service
  | Truncate.Service
  | RuntimeFlags.Service
> = Layer.effect(
  Service,
  Effect.gen(function* () {
    const config = yield* Config.Service
    const plugin = yield* Plugin.Service
    const agents = yield* Agent.Service
    const skill = yield* Skill.Service
    const truncate = yield* Truncate.Service
    const flags = yield* RuntimeFlags.Service

    const invalid = yield* InvalidTool
    const task = yield* TaskTool
    const read = yield* ReadTool
    const question = yield* QuestionTool
    const todo = yield* TodoWriteTool
    const lsptool = yield* LspTool
    const plan = yield* PlanExitTool
    const webfetch = yield* WebFetchTool
    const websearch = yield* WebSearchTool
    const repoClone = yield* RepoCloneTool
    const repoOverview = yield* RepoOverviewTool
    const shell = yield* ShellTool
    const globtool = yield* GlobTool
    const writetool = yield* WriteTool
    const edit = yield* EditTool
    const greptool = yield* GrepTool
    const patchtool = yield* ApplyPatchTool
    const skilltool = yield* SkillTool
    const agent = yield* Agent.Service
```

And the `Tool.Def` shape stored in `State.custom` / `State.builtin` (`tool.ts` L35–L45):

```ts
// upstream:packages/opencode/src/tool/tool.ts#L35-L45
export interface Def<
  Parameters extends Schema.Decoder<unknown> = Schema.Decoder<unknown>,
  M extends Metadata = Metadata,
> {
  id: string
  description: string
  parameters: Parameters
  jsonSchema?: JSONSchema7
  execute(args: Schema.Schema.Type<Parameters>, ctx: Context): Effect.Effect<ExecuteResult<M>>
  formatValidationError?(error: unknown): string
}
```

Permalinks:

- registry.ts excerpt: <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/tool/registry.ts#L60-L130>
- tool.ts Def: <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/tool/tool.ts#L35-L45>

What we kept and what we dropped:

- **Kept** — the `id` (we call it `Name`), `description`, `jsonSchema`, and `execute`. The wire format the LLM sees is byte-identical: `{name, description, input_schema}`.
- **Dropped (for now)** — Effect's Service/Layer DI plumbing (Go uses constructor injection at `main.go`), the `task`/`read` named slots (only the s10 loop cares about those), `formatValidationError` (s10 territory — translates parse errors into `tool_result{IsError:true}` Parts), `parameters` (Effect's compiled Schema decoder; we let each Go tool unmarshal into its own struct), and the entire plugin-loading path (`Glob.scanSync` of `tools/*.ts`, dynamic `import()`) — that lands in s12.
- **Forward-compat** — the per-model `tools()` filter (apply_patch vs edit gating, websearch enabled-by-provider) is an s05/s10 concern. The Registry stays neutral.

Reading order for opencode's tool layer:

1. `packages/opencode/src/tool/tool.ts` lines 35–45 — the Def interface (this s03)
2. `packages/opencode/src/tool/registry.ts` lines 60–130 — Service header (this s03)
3. `packages/opencode/src/tool/registry.ts` lines 210–260 — the built-in tool list (preview of s10)
4. `packages/opencode/src/tool/registry.ts` lines 260–350 — `tools()` per-model filter (s05 + s10)
5. `packages/opencode/src/tool/registry.ts` lines 130–210 — plugin / custom-tool loader (s12)
