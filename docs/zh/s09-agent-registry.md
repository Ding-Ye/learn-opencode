---
title: "s09 · Agent 注册表"
chapter: 9
slug: s09-agent-registry
est_read_min: 11
---

# s09 · Agent 注册表

> 本章教什么：把「一组 (model, system prompt, permission ruleset, mode)」打包成一个有名字的 Agent，构建一个 Registry 装入三个内置 agent（build / plan / general），并支持用户在 `opencode.json` 里覆写它们。核心机制是 **三层 permission 级联**：`defaults → userConfig → agentOverride`，按顺序拼接，s10 的 evaluator 走 last-match-wins —— agent 的最后一句话总能盖过前面所有层。

---

## Problem

到 s08 为止，permission rule 已经能从 `opencode.json` 加载了，但只有 *一份*：全局 `cfg.Permissions`。这盖不住实际场景：

- 用户希望同一个项目里有「正常 build」(可以 edit) 和「plan 模式」(只能 read)，两套 rule 应该共存而不是改 config 切换。
- opencode 的 `task` 工具会派一个 *subagent* 去做子任务（"去把 src/ 下面所有 TODO 注释找出来"）—— subagent 不该继承父 agent 的 edit 权限。
- 用户写自己的 agent（"researcher"，只 grep+read+websearch），希望它直接出现在 `--agent researcher` 命令里。

opencode 的解法是 **agent**：每个 agent 是 (name, mode, model, system prompt, permission ruleset) 的一个具名打包；用户在 `cfg.agent.<name>` 里声明自己的 agent，会覆写同名的内置 agent，或者新增一个。Permission 不是单一来源 —— 是 *级联* 出来的：

| 层 | 来源 | 角色 |
|---|---|---|
| defaults | 代码里的硬编码基线 | "everything 默认 allow，几个高风险的 deny" |
| userConfig | s08 的 `cfg.Permissions` | 用户全局覆写（"我所有 agent 的 edit 都先 ask"） |
| agentOverride | agent 自己的 `permissions[]` | agent 专属覆写（"plan agent 一律 deny edit"） |

按顺序排在一起，**s10 的 evaluator 走 last-match-wins**（s04 的语义） —— agent 的最后一句话总能盖过前面所有层。这就是 cascade。

直接列出：上游 `Permission.merge(defaults, fromConfig({...}), user)` 是函数签名，参数顺序就是 cascade 顺序。我们 Go 这边 `MergePermissions(defaults, userConfig, agentOverride)` 一字对应（agentOverride 排最后，因为它是 agent 自己最后说的话；upstream 那边把 user 排最后是因为 user config 是用户最后写的话；两种 ordering 表达的都是「最后一层有最终决定权」，但我们选 agent-last，因为一旦 user 已经构造出 agent 的 permissions 块，那个就是 agent 的"最后说法"）。

## Solution

类型 + 一个 Registry：

```go
type Mode int
const (ModePrimary Mode = iota; ModeSubagent; ModeAll)

type Agent struct {
    Name        string
    Mode        Mode
    Model       string
    System      string
    Permissions []Rule    // ★ 级联结果，不是原始 layer
    Tools       []string  // 可选 whitelist；nil = 所有可用工具
}

type Registry struct { agents map[string]*Agent }

func NewRegistry() *Registry           // 预装 3 个内置 agent
func (r *Registry) Register(a *Agent) error
func (r *Registry) Get(name string) (*Agent, bool)
func (r *Registry) ListByMode(m Mode) []*Agent

func MergePermissions(defaults, userConfig, agentOverride []Rule) []Rule
```

三个内置 agent，对应上游 agent.ts L122-L175：

| Agent | Mode | 角色 | Permissions（已级联） |
|---|---|---|---|
| build | Primary | 默认，可执行任何工具 | `[*:* allow]` —— 全开 |
| plan | Primary | 只读，写计划不动代码 | `read/grep/glob:* allow`，`edit/write/bash:* deny` |
| general | All | 多步探索（subagent 可用） | `*:* allow`，但 `edit/write:* ask`，`bash:rm -rf* deny` |

每个内置的 Permissions 都是 *已经 merge 过* 的最终 cascade —— 这样调用者拿到 Registry 不写一行 merge 代码就能用。需要再叠一层用户配置的，自己 `MergePermissions(defaults, userConfig, builtIn.Permissions)` 重组。

**Register 是 *整体覆写* 而不是 patch-merge**。上游 agent.ts L282-L304 的循环是 patch（每个字段从 cfg.agent 读，缺的从 built-in 继承），我们简化成全替换：用户构造 Agent 的时候自己决定从哪个 built-in 拷贝。一个简化点 —— 想要继承的人就先 `Get` 拿到 built-in、复制、改字段、再 `Register`；想完全重写的人就直接 `Register(&Agent{...})`。Patch 把"哪个字段是默认"的决定权藏在 registry 里，覆写让它显式在 caller 那里。

**`MergePermissions` 是 *扁平 concat* 而不是 dedupe**。看着可以做：把同 (Permission, Pattern) 的规则只保留最后一个，结果对 last-match-wins evaluator 是等价的。但 dedupe 会抹掉 *级联审计* —— 你看 merged slice 看不出"agent 的 deny 是覆盖了 user 的 allow 吗"。Concat 让两层都简单：这个函数纯结构性，evaluator 才做语义。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s09 三层 permission cascade                                           │
│                                                                        │
│   NewRegistry()                                                        │
│     ├─ build  (ModePrimary,  Permissions = pre-merged 全开)            │
│     ├─ plan   (ModePrimary,  Permissions = pre-merged 只读)            │
│     └─ general(ModeAll,      Permissions = pre-merged 谨慎全开)        │
│                                                                        │
│   Register(a *Agent)                                                   │
│     └─ agents[a.Name] = a   ← 同名整体覆写（含 built-in）              │
│                                                                        │
│   Get(name) → (*Agent, bool)                                            │
│   ListByMode(m) → []*Agent  ← sorted by Name                           │
│                                                                        │
│   MergePermissions(defaults, userConfig, agentOverride):                │
│     return defaults ++ userConfig ++ agentOverride                      │
│                                                                        │
│   ┌──────────────────────────────────────┐                              │
│   │  s10 evaluator 拿到 merged slice 后： │                              │
│   │    遍历，记住「最后一个匹配的 rule」  │                              │
│   │    没匹配 → ActionAsk                 │                              │
│   │    → ★ agent 排最后，所以 agent 总赢 │                              │
│   └──────────────────────────────────────┘                              │
└────────────────────────────────────────────────────────────────────────┘
```

**四个 load-bearing 决定**：

1. **cascade 顺序是 (defaults, userConfig, agentOverride) ——agent 排最后**。这条是整章的钉子。Last-match-wins 的语义意味着「排最后的层有最终决定权」；我们让 agent 排最后，因为 agent 是用户对「这个具体场景」的最后表达。如果排成 (agent, user, defaults)，那 defaults 反而能盖过 agent 的明确意图 —— `plan` agent 的 `edit:* deny` 会被 defaults 的 `edit:* ask` 改成 ask，整个 plan mode 就废了。
2. **built-in 的 Permissions 是 *已经 merge 过* 的**。`NewRegistry()` 不要求调用者懂 cascade —— `r.Get("plan")` 拿到的就是 ready-to-evaluate 的 slice。需要再叠 user config 的人主动 `MergePermissions(defaults, cfg.Permissions, builtIn.Permissions)`。让简单 case 简单，复杂 case 显式。
3. **Register 整体覆写，不 patch**。上游用 patch（聪明，但耦合 registry 跟字段默认值）；我们替换（笨，但 caller 全权决定）。结果上等价：caller 想继承就先 Get-then-mutate，想重写就直接 Register。多写两行，少一层隐式行为。
4. **ListByMode(ModePrimary) 不返回 ModeAll**。`general` 是 ModeAll —— 既能 primary 也能 subagent —— 但它*不会*偷偷出现在 `ListByMode(ModePrimary)` 里。"问什么得什么"的对称契约。要 union 就调两次。

**为什么 ~400 LOC（含测试）**：因为只做四件事 —— 三个 built-in 的字面量、Register/Get/ListByMode 的 map ops、MergePermissions 的三个 append、main.go 的演示。没做 LLM 驱动的 agent 生成（上游 `generate(...)` 在 agent.ts L321-L460），没做 prompt 模板加载（上游从 `./prompt/*.txt` 读，我们 inline 字符串），没做 explore/scout/compaction/title/summary 这些 internal-only 的 built-in。

## What Changed (vs. s04 / s08)

s04 把 ruleset 当 caller 直接构造的 in-memory literal；s08 把 ruleset 移到了 `Config.Permissions`，从 `opencode.json` 加载；s09 把 Permission 装进 *agent 的 context*：

```diff
 // s04: 单一 ruleset，inline literal。
-Evaluate("edit", "main.go", Ruleset{
-  {Permission: "edit", Pattern: "*.go", Action: ActionAllow},
-})

 // s08: ruleset 来自 Config，但还是单一来源。
-cfg, _ := Load(cwd, homeDir, EnvFromOS())
-Evaluate("edit", "main.go", cfg.Permissions)

+// s09: ruleset 装在 agent 里，cascade 出来。
+r := NewRegistry()
+plan, _ := r.Get("plan")
+merged := MergePermissions(defaults, cfg.Permissions, plan.Permissions)
+// s10 拿 merged 给 evaluator: Evaluate("edit", "main.go", merged)
+// → 走 last-match-wins，plan 的 deny 总赢 user 的 allow。
```

`Rule` 的形状一行没改 —— s04 决定「Rule 是 dumb data」继续兑现红利。s08 加的是 *来源*，s09 加的是 *上下文（哪个 agent）* + *级联组合（三层）*，都没动 Rule shape。对 `Action` enum 的 unmarshal 行为也不变：JSON 里 typo 仍然 fallback 到 `ActionAsk`（fail-closed）。

s10 接下来要做的事：拿到 merged slice 后，把它喂给 s04 的 `Evaluate(permission, target, mergedSlice)`。Evaluator 自己不知道有"三层"这回事 —— 它只看到一个扁平的 `[]Rule`，按顺序找最后匹配的。这就是 *cascade 是结构性、evaluator 是语义性* 这条分工的回报：两边都不需要知道对方的细节。

## Try It

```bash
cd agents/s09-agent-registry

# 演示（确定性，无网络）：
go run .

# 4 个测试：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

4 个测试覆盖的场景：

1. **BuiltinAgentsResolve** —— `Get("build" | "plan" | "general")` 都返回非空，Mode 正确，Permissions 非空。"未知 agent" 返回 `ok=false`，让 caller 能区分「没配」和「配了但全空」。
2. **UserDefinedAgentOverridesBuiltin** —— 注册同名 `build` 但用 `openai/gpt-4o` 模型，`Get("build").Model` 必须变成 gpt-4o（整体覆写，不是 patch）。也钉了「nil / 空 Name → error」的输入校验。
3. **MergePermissionsConcatenatesInOrder** —— 钉字面 concat 输出（defaults ++ user ++ agent），并验证返回的 slice 是独立的（mutate 不影响输入）。空输入 → nil。
4. **ListByModeReturnsOnlyMatching** —— primary 集合恰好 `{build, plan}`；subagent 在 Register 之前是空的，Register 一个 ModeSubagent 之后出现；ModeAll 列表只有 general（不被 ModePrimary 列表"自动提升"）。

## Upstream Source Reading

s09 mirror 的是 opencode 的 `packages/opencode/src/agent/agent.ts`。整个文件 460 行，s09 取的是 *runtime registry* 那一段（L28-L304）—— Schema、defaults ruleset、三个核心 built-in、cfg.agent 覆写循环。剩下的 `generate(...)` LLM 驱动的 agent 合成（L321-L460）是另一个机制（"用 LLM 写一个新 agent"），s09 不做。

```ts
// upstream:packages/opencode/src/agent/agent.ts L28-L48 + L100-L175 + L282-L304

// L28-L48 — 运行时 Info schema。我们 Go 这边 `Agent` struct 留 6 个
// load-bearing 字段（name, mode, model, prompt, permission, tools），
// 砍掉渲染相关的（color, temperature, topP, variant, hidden, native,
// options, steps）—— 后者不影响"agent 能干什么"的语义。
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  mode: Schema.Literals(["subagent", "primary", "all"]),
  // ...
  permission: Permission.Ruleset,  // ★ 这是 cascade 的最终结果
  model: Schema.optional(Schema.Struct({ modelID: ModelID, providerID: ProviderID })),
  prompt: Schema.optional(Schema.String),
  // ...
})

// L100-L121 — defaults 和 user 两层，每个 built-in 的
// Permission.merge(defaults, ..., user) 都用它们。
const defaults = Permission.fromConfig({
  "*": "allow",
  doom_loop: "ask",
  external_directory: { "*": "ask" /* + skill dir whitelisting */ },
  question: "deny",
  plan_enter: "deny",
  plan_exit: "deny",
  // ...一些更细的 read 规则
})
const user = Permission.fromConfig(cfg.permission ?? {})

// L122-L175 — 三个核心 built-in。注意每个的 permission 都是
// `Permission.merge(defaults, agentSpecific, user)` 的三层级联。
const agents = {
  build: {
    name: "build",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({ question: "allow", plan_enter: "allow" }),
      user,
    ),
    mode: "primary",
    native: true,
  },
  plan: {
    name: "plan",
    description: "Plan mode. Disallows all edit tools.",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({
        edit: { "*": "deny" /* + plan markdown 白名单 */ },
        // ...
      }),
      user,
    ),
    mode: "primary",
    native: true,
  },
  general: {
    name: "general",
    permission: Permission.merge(
      defaults,
      Permission.fromConfig({ todowrite: "deny" }),
      user,
    ),
    mode: "subagent",
    native: true,
  },
}

// L282-L304 — cfg.agent 覆写循环。每个用户声明的 agent：
//   - 已存在 built-in → patch（field-by-field merge）
//   - 不存在 → 新建（permission = Permission.merge(defaults, user) 两层）
// 我们 Go 这边简化成 Register 整体覆写：caller 自己决定从哪继承。
for (const [key, value] of Object.entries(cfg.agent ?? {})) {
  if (value.disable) { delete agents[key]; continue }
  let item = agents[key]
  if (!item) item = agents[key] = {
    name: key, mode: "all",
    permission: Permission.merge(defaults, user),
    options: {}, native: false,
  }
  if (value.model) item.model = Provider.parseModel(value.model)
  // ...其它字段一一 patch...
  item.permission = Permission.merge(item.permission, Permission.fromConfig(value.permission ?? {}))
}
```

逐行注释（重点行）：

- **L28-L48 `Info` schema** —— 运行时 agent 的 *形状*。我们的 `Agent` struct 是它的子集（去掉了 8 个不影响行为的字段）。`mode` 是 `"primary" | "subagent" | "all"` 的字符串字面量；我们用 `Mode` enum + `String()` 方法。`permission` 字段类型是 `Permission.Ruleset`（一个 `Rule[]`），我们对应 `Permissions []Rule`。
- **L100-L121 defaults / user 两层** —— 每个 built-in 都用同一份 `defaults`（硬编码安全基线）和同一份 `user`（来自 `cfg.permission`）。我们 Go 这边对应：调用者自己构造 defaults []Rule（demo 里就这么做），cfg.Permissions 来自 s08。
- **L128 build 的 cascade** —— `Permission.merge(defaults, fromConfig({question: "allow", plan_enter: "allow"}), user)`。三层：defaults → build 自己加的两条 → user。我们的 `MergePermissions(defaults, userConfig, agentOverride)` 顺序是 `(defaults, user, agent)` —— **agent 排最后** 而不是 user 排最后。两种 ordering 都满足"最后一层胜"；我们选 agent-last，因为 cfg.agent.<name>.permission 已经是 user 对 agent 最后的明确意图（user 写在 cfg.agent 里的 permission 块就是 agent 的"最终说法"）。
- **L143-L161 plan 的 cascade** —— 同样三层，agent-specific 这层的内容是 `edit: {"*": "deny", ...}` —— 把所有 edit 全 deny 了，然后只白名单 `.opencode/plans/*.md` 这种计划文件。我们 plan built-in 的 Permissions 化简了：直接 `read/grep/glob:* allow` + `edit/write/bash:* deny`，把白名单跳过。
- **L162-L175 general 的 cascade** —— 注意 mode 是 `"subagent"`（只在 task 工具的子调用里用）。我们的 general 是 ModeAll（更宽松，既能 primary 也能 subagent），因为 s09 没装 task 工具，让 general 当 ModeAll 既能在 demo 里看到又不会语义错。
- **L282-L304 cfg.agent 覆写循环** —— 这是 s09 `Register` 对应的上游代码。看 L286-L290：`if (!item) item = agents[key] = { ... permission: Permission.merge(defaults, user) }` —— 新增的 agent，permission 是 *两层*（defaults + user，没有 agent-specific 第三层，因为 agent 还没声明自己的 rules）。然后 L303 `item.permission = Permission.merge(item.permission, Permission.fromConfig(value.permission ?? {}))` 才把 agent 自己的 permissions 合并进去 —— 这是 *第三层*。我们 Go 这边把这个责任推给 caller：构造 Agent 时自己决定 `Permissions: MergePermissions(defaults, user, ownRules)`。

permalink：

- Info schema (L28-L48): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L28-L48>
- defaults + user 层 (L100-L121): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L100-L121>
- 三个核心 built-in (L122-L175): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L122-L175>
- cfg.agent 覆写循环 (L282-L304): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/agent/agent.ts#L282-L304>

我们留下了什么、砍了什么：

- **留下** —— Info schema 的 6 个 load-bearing 字段、三个核心 built-in (build/plan/general)、三层 cascade 语义、cfg.agent 覆写（Register 整体覆写形式）、ModePrimary/Subagent/All 三种 mode、ListByMode 过滤。
- **暂时砍掉** —— explore / scout / compaction / title / summary 这五个 built-in（前两个是探索向，后三个是 internal-only），LLM 驱动的 `generate(...)` agent 合成（agent.ts L321-L460），Truncate.GLOB 自动白名单后处理（L307-L320），prompt 文件加载（我们 inline 字符串），field-by-field patch-merge（用整体 Register 替代）。
- **向前兼容** —— `Agent` 加新字段（比如 temperature / topP）不破坏 cascade 逻辑，因为 cascade 只动 Permissions 一个字段。s10 拿 cascade 后的 slice 直接喂 evaluator，对增加字段透明。后续如果要做 patch-merge（s09 的简化往回走一步），加一个 `RegisterMerged(name string, override AgentOverride)` 不影响现有 Register。

opencode agent 层的阅读顺序：

1. `packages/opencode/src/agent/agent.ts` L28-L48 —— `Info` Schema（s09 的 Agent struct 母本，本节正文）
2. `packages/opencode/src/agent/agent.ts` L100-L175 —— defaults / user / 三个核心 built-in 的 cascade（s09 mirror 的核心）
3. `packages/opencode/src/agent/agent.ts` L282-L304 —— cfg.agent 覆写循环（s09 的 Register 简化版）
4. `packages/opencode/src/permission/index.ts` —— `Permission.merge` / `Permission.fromConfig` 的实现（s09 的 MergePermissions 是这两个的合并简化版）
5. `packages/opencode/src/permission/evaluate.ts` L9-L15 —— `findLast` 的 last-match-wins 语义（s10 拿 s09 的 cascade slice 喂这个 evaluator）
