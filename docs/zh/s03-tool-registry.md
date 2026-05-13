---
title: "s03 · 工具注册表"
chapter: 3
slug: s03-tool-registry
est_read_min: 10
---

# s03 · 工具注册表

> 本章教什么：Part union 的 *生产者* 这一面。s02 把 `PartToolUse` 建模成「我们 *接收* 的数据」。s03 造出来 *回应* 它的注册表 —— 给一个工具名 + JSON 参数，跑代码，返回下一个 `tool_result` Part 要装的字符串。这个接口钉准了，后面所有节（权限门、流式调度、MCP、LSP）都直接往上插。

---

## Problem

s02 收尾时已经能从 wire 上拿到一个带类型的 `PartToolUse{ID, Name, Input}`。那么……谁来跑它？

真实 agent 至少十几个工具：`read`、`write`、`edit`、`grep`、`shell`、`glob`、`webfetch`、`task`、`todo`、`skill`、`lsp`...... opencode 的 `packages/opencode/src/tool/registry.ts` 有 430 行，是因为它把按模型过滤、插件加载、telemetry、DI 全都缝在一起。但下面其实只有一件事要做：

> 给一个 `tool_use.name`，返回一个 callable —— 它接受 LLM 的 JSON 参数，产出下一个 `tool_result` 要装的字符串。

如果这个接口 *现在* 不结晶下来 —— 在 s10 引入流式调度循环、s12 引入远程 MCP 工具、s04 引入权限门之前 —— 后面每节都要自己另搞一个临时 map，根本组合不起来。

## Solution

三块，~120 行 Go：

1. **`Tool` 接口**，4 个方法：`Name()`、`Description()`、`JSONSchema()`、`Execute(ctx, json.RawMessage) (string, error)`。前三个是 LLM 在自己工具列表里看到的；第四个是 LLM 选了这个工具时 loop 调用的闭包。
2. **`Registry` struct**，里面是 `map[string]Tool`。`Register` 添加；`Lookup` 读（comma-ok）；`Names` 返回有序 key；`ToolSchemas()` 投影到 LLM 看的 wire 形状；`Dispatch` 就是 lookup-then-execute 这一行。
3. **两个内置工具** 用于测试：`EchoTool`（原样返回输入）和 `NowTool`（返回 time.Now，可选格式）。极小且确定 —— 正是 `tool_test.go` 需要的。

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│  s03 工具注册表                                                  │
│                                                                 │
│   启动期：                                                       │
│     reg := NewRegistry()                                        │
│     reg.Register(EchoTool{})  ─┐                                │
│     reg.Register(NowTool{})   ─┴─►  reg.tools = {echo:_, now:_} │
│                                                                 │
│   把 tools 发给 LLM（s05 的 Provider 之后会拼这一段）：             │
│     reg.ToolSchemas() ─► [{name, description, input_schema},..] │
│                                                                 │
│   收到 LLM 发来的 tool_use Part（s10 的 loop）：                  │
│     PartToolUse{Name:"echo", Input:{"text":"hi"}}               │
│            │                                                    │
│            ▼                                                    │
│     reg.Dispatch(ctx, "echo", input)                            │
│            │                                                    │
│            ├─► Lookup("echo") → EchoTool{}                      │
│            └─► EchoTool.Execute(ctx, input)                     │
│                                                                 │
│            ► 返回 "hi"                                          │
│                  │                                              │
│                  ▼                                              │
│     PartToolResult{ToolUseID:"...", Content:"hi", IsError:false}│
└─────────────────────────────────────────────────────────────────┘
```

`tool.go` 里的接口：

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
    return t.Execute(ctx, input)  // (带错误包装)
}
```

**三个不显而易见的点**：

1. **input 用 `json.RawMessage`，不用 typed struct。** Tool 接口不可能知道每个具体工具的 input 形状。每个工具自己 `json.Unmarshal` 到自己私有的 input struct。代价是每次 dispatch 多一次 `json.Unmarshal`。收益是接口在 14 节里只有一个签名，新增工具永远不需要碰 Registry。
2. **`ToolSchemas()` 返回 `[]map[string]any`，不是 `[]ToolSchema`。** 看着像偷懒；其实是有意：s05 的 Provider 会在自己那一层把 OpenAI 的 `function` 信封（或 Bedrock 的 `toolSpec`）拼上去，typed Go struct 会逼出一次反射或者第二个翻译器。`any` 的 bag 是 provider-neutral 的。
3. **`ErrUnknownTool` 是 sentinel。** `errors.Is(err, ErrUnknownTool)` 让 s10 的 loop 能在 LLM 幻觉出工具名时把它翻译成合成的 `tool_result{IsError:true}` Part —— 而不是崩。错误消息里同时带了 bad tool 名和已注册集合，操作员扫日志一眼就能修。

## What Changed (vs. s02)

s02 加了 `Part` union。`PartToolUse` 这个 arm 装着 `{Name, Input}` 但消费侧没有任何东西知道这个 name 该 *做* 什么。s03 把这个洞补上：

```diff
 // s02: Part 可以接收和读取。
 type Part struct {
     Kind    PartKind
     ToolUse *ToolUseRef  // {ID, Name, Input}
     ...
 }

+// s03: Part 现在可以被生产了。给一个 PartToolUse，Registry 回答下一个
+// PartToolResult 该装什么字符串。
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

s02 的 wire-format 工作完全没动。s03 只 *加* —— 从来不重读、不修改 Part。这种干净的生产者/消费者切分，就是后面 s10（流式 loop）能把两者组合起来而双方互不知道彼此的关键。

## Try It

```bash
cd agents/s03-tool-registry

# Demo：注册 2 个内置，打印 LLM 看的 schemas，分别 dispatch。
go run .

# 4 个测试，无网络，无真实时钟（NowTool 用可注入的 nowFn）。
go test -count=1 ./...

# 只看 LLM 看到的 JSON schemas：
go run . | sed -n '/tool_schemas/,/dispatch/p' | head -40
```

## Upstream Source Reading

s03 镜像的机制在 opencode 的 `packages/opencode/src/tool/registry.ts`（Service 定义）加 `packages/opencode/src/tool/tool.ts`（Def 接口）。下面是 registry.ts 的 State + Interface + Service 头，合起来就是上游对「一个 registry 装什么」这个问题的回答 —— LLM 看到的每个工具都从这个 Service 的 `all()` / `tools()` 走。

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

以及存在 `State.custom` / `State.builtin` 里面的 `Tool.Def` 形状（`tool.ts` L35–L45）：

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

Permalink：

- registry.ts excerpt：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/tool/registry.ts#L60-L130>
- tool.ts Def：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/tool/tool.ts#L35-L45>

我们留下了什么、砍了什么：

- **留下** —— `id`（我们叫 `Name`）、`description`、`jsonSchema`、`execute`。LLM 看的 wire 形状字节级一致：`{name, description, input_schema}`。
- **暂时砍掉** —— Effect 的 Service/Layer DI 管线（Go 在 `main.go` 里直接构造注入）、`task`/`read` 命名 slot（只有 s10 的 loop 用得上）、`formatValidationError`（s10 的地盘 —— 把 parse error 翻译成 `tool_result{IsError:true}` Part）、`parameters`（Effect 编译过的 Schema decoder；我们让每个 Go 工具自己 unmarshal 到自己的 struct）、整套 plugin 加载路径（`Glob.scanSync` 扫 `tools/*.ts`，动态 `import()`）—— 这块在 s12 落地。
- **向前兼容** —— 按模型过滤的 `tools()`（apply_patch vs edit、websearch 按 provider 启用）是 s05/s10 的事。Registry 保持中性。

opencode 工具层的阅读顺序：

1. `packages/opencode/src/tool/tool.ts` 35–45 行 —— Def 接口（本节 s03）
2. `packages/opencode/src/tool/registry.ts` 60–130 行 —— Service 头（本节 s03）
3. `packages/opencode/src/tool/registry.ts` 210–260 行 —— 内置工具列表（s10 预告）
4. `packages/opencode/src/tool/registry.ts` 260–350 行 —— `tools()` 按模型过滤（s05 + s10）
5. `packages/opencode/src/tool/registry.ts` 130–210 行 —— plugin / custom-tool loader（s12）
