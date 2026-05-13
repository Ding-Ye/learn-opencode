// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/tool/registry.ts (excerpt — State, Interface, Service, layer head)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/tool/registry.ts
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s03 cares about this file:
//   This IS opencode's tool registry. The two halves we mirror in Go:
//     1. The shape of a tool definition  → our `Tool` interface (tool.go)
//     2. The collection that holds them  → our `Registry` struct (tool.go)
//   Everything else in this 430-line upstream file (plugin loading, custom
//   directories, per-model filtering, AI-SDK telemetry) layers on top of those
//   two primitives. Get them right and the rest is a matter of slotting in
//   later sessions: s04 (permission-filtered tools), s12 (MCP-loaded tools),
//   s10 (the streaming dispatch loop).
//
// What we rebuilt in Go (s03):
//   - `Tool.Def` (id/description/parameters/jsonSchema/execute)
//        → `Tool` interface { Name, Description, JSONSchema, Execute }
//   - `State { custom, builtin, task, read }`
//        → `Registry { tools map[string]Tool }` (we collapse custom+builtin;
//          the `task`/`read` named slots only matter once s10 needs them)
//   - `Interface { ids, all, named, tools }`
//        → Registry methods { Names, Lookup, ToolSchemas, Dispatch }
//   - `Tool.init(...)` wrapper that compiles the parser closure
//        → tool itself owns its `json.Unmarshal` step (one fewer layer)
//
// What we DID NOT rebuild yet:
//   - Effect's `Layer` / `Context.Service` DI plumbing — Go uses constructor
//     injection at main.go; no equivalent needed at s03's scope.
//   - Plugin loading (`fromPlugin`, `Glob.scanSync`, dynamic `import()`) —
//     plugins land in s12 (MCP) and s11 (skills) where it makes more sense.
//   - Per-model `tools()` filter (apply_patch vs edit gating, websearch
//     enabled-by-provider) — that's an s05 (Provider) and s10 (loop) concern.
//   - `formatValidationError` hook on Tool.Def — s10 wires this when it
//     translates Execute errors into `tool_result{IsError:true}` Parts.
//
// ---- begin upstream excerpt (lines 60–130 of registry.ts) ----

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

// ---- and the Tool.Def shape that Service stores (tool.ts L35–L45) ----

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

// ---- end upstream excerpt ----
//
// Reading map (do these in s03 order — later sessions read deeper):
//   1. tool.ts L35–L45 (Def interface)        — the "what is a Tool" question
//   2. registry.ts L60–L130 (State + Service) — the "where do Tools live" question
//   3. registry.ts L210–L260 (builtin list)   — what 17 tools opencode ships
//   4. registry.ts L260–L350 (`tools()`)      — per-model tool filtering (s10)
//   5. registry.ts L130–L210 (plugin loader)  — custom-tool ingestion (s12)
//
// The mental jump from upstream → s03 Go:
//   - Effect.Service / Layer DI                         → constructor in main.go
//   - State{custom, builtin, task, read}                → flat map[string]Tool
//   - Tool.Def execute returning Effect<ExecuteResult>  → Execute returns (string, error)
//   - Schema.decodeUnknownEffect parsing args           → tool's own json.Unmarshal
// The wire format the LLM sees is identical: {name, description, input_schema}.
