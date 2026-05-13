---
title: "s08 · 配置加载"
chapter: 8
slug: s08-config-load
est_read_min: 11
---

# s08 · 配置加载

> 本章教什么：把 agent 的所有可配置旋钮（model、permission rules、instruction 文件路径、MCP server 列表、LSP server 列表、skill 目录）从代码硬编码搬到 `opencode.json`。三层来源 —— project（cwd 向上找 `.opencode/opencode.json`）、user（`~/.opencode/opencode.json`）、env override（`OPENCODE_MODEL` 等）—— 按确定顺序 deep merge。`Instructions[]` 是 *concat*，其它字段都是 *override*。纯 stdlib，无依赖。

---

## Problem

到 s07 为止，agent 的可配置项基本都活在代码里：

- s04 的 permission ruleset 是测试里 inline literal `[]Rule{{...}}`。
- s05 的 provider/model 在 `main` 里硬编码 `"claude-3-5-sonnet"`。
- s06 / s07 暂时没有需要配置的东西。

但只要往后走一步：
- s09 要按 agent name 查 model + permission cascade —— 用户得能在 `opencode.json` 里写「我这个 build agent 用 sonnet，scout agent 用 haiku」。
- s10 工具循环要查 permission —— 规则得从 *配置* 来，不能 hardcode。
- s11 SKILL.md 发现要知道扫描哪些目录 —— 来源是 config 的 `skills.paths`。
- s12 / s13 要起 MCP / LSP 子进程 —— 起哪些来源是 config。

opencode 的配置布局是熟悉的两层 + env override：

| 层 | 文件 | 角色 |
|---|---|---|
| project | `<cwd 或祖先>/.opencode/opencode.json` | 当前项目的覆写；进入子目录时自动找到 |
| user    | `~/.opencode/opencode.json`            | 用户全局默认；所有项目共享 |
| env     | `OPENCODE_PROVIDER` / `OPENCODE_MODEL` | 单次运行的临时覆写 |

合并规则不是「project 整个替换 user」—— 是 **deep merge**：`Provider.ModelID` 被项目改了，但 `Provider.ProviderID` 用户设的还在。`Skills` 是 map，两边的 key union。最关键的例外：**`Instructions[]` 是 concat（去重），不是替换** —— 用户全局的 `~/CLAUDE.md` 不能因为项目加了一行 `AGENTS.md` 就消失。

为什么不引 `mergo` 之类的库？因为 deep merge 的语义不是「我要 mergo 默认行为」—— 是「我要 *明确* 每个字段是 override 还是 concat」。把 30 行 `mergeConfigs` 写出来比依赖一个库 + 在 docs 里解释「我们用了 `mergo.WithAppendSlice` 但 Instructions 例外」要简单太多。

## Solution

类型 + 一个 `Load` 入口：

```go
type Config struct {
    Provider     ProviderConfig
    Agents       []AgentConfig
    Permissions  []Rule         // s04 的 Rule，本模块本地重定义
    Instructions []string       // ★ concat 字段
    LSP          map[string]LSPConfig
    MCP          []MCPConfig
    Skills       map[string]string
}

func Load(cwd, homeDir string, env map[string]string) (*Config, error)
```

三步 pipeline：

1. **user**：读 `<homeDir>/.opencode/opencode.json{,c}`（不存在 → 空 Config，不报错）
2. **project**：从 cwd 向上走，找到第一个 `.opencode/opencode.json{,c}` 就停（同样：找不到 → 空 Config）
3. **env**：在合并结果上 in-place 应用 `OPENCODE_PROVIDER` / `OPENCODE_MODEL`

合并：`mergeConfigs(user, project)` —— 顺序是 user 在前、project 在后，所以 project override user。env 最后应用，所以 env override 一切。

**`mergeConfigs` 的 7 条规则**：

| 字段 | 规则 |
|---|---|
| `Provider.{ProviderID,ModelID}` | 各自 override-if-non-empty（per-field 而不是整个 struct 替换） |
| `Agents` | override-if-non-empty 的 slice 整体替换 |
| `Permissions` | override-if-non-empty 的 slice 整体替换 |
| `Instructions` | **base ++ override，去重（保留首次出现）** |
| `LSP` | map union，override 的 key 胜 |
| `MCP` | override-if-non-empty 的 slice 整体替换 |
| `Skills` | map union，override 的 key 胜 |

**JSONC 支持**：opencode 允许在 `opencode.jsonc` 里写 `// 行注释` / `/* 块注释 */`。我们用一个 30 行的状态机 `stripJSONC` 把注释剥掉再丢给 `json.Unmarshal`。状态机把字符串字面量当一等公民处理，所以 `"key": "// not a comment"` 不会被误伤。零依赖。

**为什么 walk upward**：用户 `cd ~/projects/foo/sub/dir` 然后 `opencode`，预期是 `~/projects/foo/.opencode/opencode.json`（如果存在）能生效，而不是要求每个子目录都放一份配置。我们从 cwd 开始，每次 `filepath.Dir`，直到根。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s08 三层 config 合并                                                   │
│                                                                        │
│   Load(cwd, homeDir, env)                                               │
│     │                                                                  │
│     ├─ user     ←  loadOptional(<homeDir>/.opencode/opencode.{jsonc,json})│
│     │             ↓ 不存在 → Config{}                                   │
│     │                                                                  │
│     ├─ project  ←  walk-up from cwd → first `.opencode/opencode.*` hit  │
│     │             ↓ 找不到 → Config{}                                   │
│     │                                                                  │
│     └─ merged   =  mergeConfigs(user, project)  ← project 覆盖 user      │
│                                                                        │
│           applyEnvOverrides(&merged, env)        ← env 覆盖一切         │
│                                                                        │
│           return &merged                                                │
│                                                                        │
│   mergeConfigs(base, override)：                                         │
│     - 标量 / 嵌套 struct 的子字段 → override 非空则胜                    │
│     - slice（Agents / Permissions / MCP）→ override 非空则整个替换       │
│     - Instructions[] → base ++ override，dedup 保留首次                  │
│     - map（LSP / Skills）→ shallow union，override 胜                    │
└────────────────────────────────────────────────────────────────────────┘
```

**四个 load-bearing 设计**：

1. **「override-if-non-empty」语义。** Go 的零值（`""` / `nil`）扮演了 TS 的 `undefined` 角色 —— 一份没设 `model` 的配置 *不* 应该把上一层的 `model` 抹成空字符串。这条规则让 user 配置可以只关心「我要全局默认什么」，project 配置只写「我和默认不同的部分」，不需要互相 copy 全部字段。
2. **`Instructions[]` 是 concat 例外。** 上游 `mergeConfigConcatArrays` 用 `Array.from(new Set([...target, ...source]))`；我们用 `dedupStrings(append(base, override...))`。语义完全一样。如果换成 override 替换，用户的 `~/CLAUDE.md` 会因为 project 加了一个 `AGENTS.md` 而消失 —— 系统提示词碎片的 silent data loss，最坏情况。
3. **walk upward 必须停在文件系统根。** `filepath.Dir("/") == "/"`（macOS / Linux），`filepath.Dir("C:\\") == "C:\\"`（Windows）—— 不写终止条件就死循环。我们用 `parent == dir` 跳出，这是 Go 跨平台 walk-up 的成语。
4. **env override 在 merge *之后* 应用。** 如果在 merge 前应用，project 的 `model` 会再次覆盖 env 设的 `model`，env 就形同虚设。顺序是 load-bearing 的；写测试 #5 那条「设了 env 之后无 env 的 fallback」就是钉这一点。

**为什么是 ~450 LOC（含测试）**：因为只做四件事 —— 找文件、读 JSON、merge、应用 env。没用 schema validator（`encoding/json` 的 struct tag 够），没用 mergo（手写 30 行更明确），没用 jsonc-parser（30 行状态机够），没有 plugin 解析 / `${VAR}` 模板 / 文件 watcher（都在更后面的章节或范围外）。

## What Changed (vs. s04)

s04 的 ruleset 是 test 里 inline 写死的 `[]Rule{{Permission: "edit", ...}}`。s08 的 ruleset 来自 `Config.Permissions`，从 `opencode.json` 加载：

```diff
 // s04: ruleset 是 caller 直接构造的 in-memory literal。
 ruleset := Ruleset{
-    {Permission: "edit", Pattern: "*.go", Action: ActionAllow},
-    {Permission: "bash", Pattern: "rm -rf*", Action: ActionDeny},
 }
-Evaluate("edit", "main.go", ruleset)
+
+// s08: ruleset 来自 Config，Config 从盘上加载。
+cfg, _ := Load(cwd, homeDir, EnvFromOS())
+// s10 把 cfg.Permissions 喂给 evaluate；s09 在每个 agent 的
+// AgentConfig.Permissions 之外再 cascade 一层。
+for _, r := range cfg.Permissions {
+    fmt.Printf("%s %s -> %s\n", r.Permission, r.Pattern, r.Action)
+}
```

`Rule` 的形状一行没改 —— 这是 s04「Rule 是 dumb data」做对了的证明。s08 加的是 *来源* 这一层，没动 *形状*。`UnmarshalJSON` 把 `"allow"` / `"deny"` / `"ask"` 字符串映射到 `Action` enum；任何其它值（包括缺失）都 fallback 到 `ActionAsk`，让 typo 失败的方向是「问用户」而不是「悄悄放行」。

s09 会在这之上再加一层 cascade：`globalPermissions ⊕ userOverridePermissions ⊕ agentOwnPermissions`，按先后顺序 evaluate（last-match-wins —— 还是 s04 的语义）。s08 这里只负责把 `globalPermissions` 这第一层从 JSON 拿出来。

## Try It

```bash
cd agents/s08-config-load

# 演示（确定性，无网络）：
go run .

# 5 个测试：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

5 个测试覆盖的场景：

1. **ProjectOnlyConfig** —— homeDir 给空（= 没有 user config），只有 project 的 `.opencode/opencode.json`。Provider / Instructions / Permissions 都从 project 出。
2. **UserOnlyConfig** —— cwd 没有任何 `.opencode/`，只有 `~/.opencode/opencode.json`。所有字段从 user 出。
3. **ProjectOverridesUser** —— 两边都设 `Provider.ModelID`，project 胜；user 设的 `Skills` map 因为 project 没碰，原样保留。验证 *per-field* deep merge（不是 whole-struct 替换）。
4. **InstructionsConcatenated** —— user `[~/CLAUDE.md, shared.md]` + project `[AGENTS.md, shared.md]` → 合并 `[~/CLAUDE.md, shared.md, AGENTS.md]`（user 在前，shared.md 去重）。
5. **EnvOverrideOfProviderModel** —— `OPENCODE_MODEL=claude-3-opus` 胜过两个文件里的设置；同时验证「无 env 时」project 仍然胜过 user，确认 env override 是 *额外* 一层而不是替换 merge 顺序。

## Upstream Source Reading

s08 mirror 的是 opencode 的 `packages/opencode/src/config/config.ts`。整个文件 1500+ 行，但 deep-merge + array-concat 的核心只有三个小函数（L49-L110）—— 我们 Go 那边一字不改地把语义照搬。schema 部分（L120-L292，30+ 字段）我们只取 7 个字段子集，剩下的等对应 session 落地再加。

```ts
// upstream:packages/opencode/src/config/config.ts L49-L110

// Custom merge function that concatenates array fields instead of replacing them
// Keep remeda's deep conditional merge type out of hot config-loading paths;
// TS profiling showed it dominates here.
function mergeConfig(target: Info, source: Info): Info {
  return mergeDeep(target, source) as Info
}

function mergeConfigConcatArrays(target: Info, source: Info): Info {
  const merged = mergeConfig(target, source)
  if (target.instructions && source.instructions) {
    merged.instructions = Array.from(new Set([...target.instructions, ...source.instructions]))
  }
  return merged
}

function normalizeLoadedConfig(data: unknown, source: string) {
  if (!isRecord(data)) return data
  const copy = { ...data }
  const hadLegacy = "theme" in copy || "keybinds" in copy || "tui" in copy
  if (!hadLegacy) return copy
  delete copy.theme
  delete copy.keybinds
  delete copy.tui
  log.warn("tui keys in opencode config are deprecated; move them to tui.json", { path: source })
  return copy
}

async function substituteWellKnownRemoteConfig(input: { value: unknown; dir: string; source: string }) {
  if (!isRecord(input.value) || typeof input.value.url !== "string") return
  // ...remote config URL substitution; out of scope for s08...
}

async function resolveLoadedPlugins<T extends { plugin?: ConfigPlugin.Spec[] }>(config: T, filepath: string) {
  if (!config.plugin) return config
  for (let i = 0; i < config.plugin.length; i++) {
    config.plugin[i] = await ConfigPlugin.resolvePluginSpec(config.plugin[i], filepath)
  }
  return config
}
```

逐行注释（重点行）：

- **L49-L51 `mergeConfig`** —— 一行包装 `remeda.mergeDeep`。注释提到「TS profiling 显示这是热路径，绕过 remeda 的 deep-conditional 类型推导」。Go 那边没这问题（手写循环）；我们的 `mergeConfigs` 等价做法是手列每个字段的合并规则。
- **L53-L59 `mergeConfigConcatArrays`** —— 这是 s08 的 *核心*。先 `mergeConfig` 走默认 deep-merge，然后 *单独* 处理 `instructions`：用 `new Set([...A, ...B])` 拼起来去重。我们的 Go 版本（`config.go` 里的 `mergeConfigs` Instructions 分支）做的是同一件事 —— `dedupStrings(append(base.Instructions, override.Instructions...))`。`Set` 在 JS 里保留插入顺序，我们的 `dedupStrings` 用 map-as-set + 输出 slice 也保留首次出现顺序，行为一致。
- **L61-L71 `normalizeLoadedConfig`** —— legacy key 剥离。`theme` / `keybinds` / `tui` 当年在 opencode.json 里，后来挪到了 sibling 的 `tui.json`。这个 fn 在加载时把它们删掉并 `log.warn`。Go 那边我们的 `Config` struct 根本没声明这些字段 —— `encoding/json` 默认行为是「未知字段 silent drop」—— 所以这一步是 no-op，我们不实现。
- **L73-L101 `substituteWellKnownRemoteConfig`** —— 处理 `{url, headers}` 形式的远程配置 include（让一个 config 文件包另一个 URL）。s08 不做 include / 远程加载，纯本地。
- **L103-L111 `resolveLoadedPlugins`** —— 把 `{ plugin: [...] }` 里的相对路径解析成绝对路径，避免后续 merge 移位时丢失上下文。s08 不做 plugin（opencode 的 plugin 系统是另一个大题），略。

`Info` schema 的 partial 列表（L120-L292，节选我们用到的字段）：

```ts
export const Info = Schema.Struct({
  // ...
  provider: Schema.optional(Schema.Record(Schema.String, ConfigProvider.Info)),  // 我们简化成 {ProviderID, ModelID}
  agent:    Schema.optional(/* ... */),                                          // 我们的 Agents []AgentConfig
  permission: Schema.optional(ConfigPermission.Info),                            // 我们的 Permissions []Rule
  instructions: Schema.optional(Schema.mutable(Schema.Array(Schema.String))),     // 我们的 Instructions []string ★ concat
  lsp:    Schema.optional(ConfigLSP.Info),                                       // 我们的 LSP map
  mcp:    Schema.optional(/* ... */),                                            // 我们的 MCP []MCPConfig
  skills: Schema.optional(ConfigSkills.Info),                                    // 我们的 Skills map
  // ...另外 23 个字段，s08 暂未实现，每个对应未来 session
})
```

permalink：

- mergeConfig + mergeConfigConcatArrays（L49-L60）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L49-L60>
- normalizeLoadedConfig + 远程 / plugin（L61-L111）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L61-L111>
- 完整 Info schema（L120-L292）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L120-L292>
- paths.ts（walk-up 搜索）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/paths.ts>

我们留下了什么、砍了什么：

- **留下** —— 三层来源（user / project / env）、deep merge 语义、`Instructions[]` concat 例外、JSONC 支持、walk-upward 项目搜索、`OPENCODE_CONFIG_DIR` / `OPENCODE_PROVIDER` / `OPENCODE_MODEL` env hooks。
- **暂时砍掉** —— `${VAR}` 字符串模板（s11 skill discovery 那边再做）、远程 config include、plugin 解析、`$schema` 自动注入、`tui.json` legacy 迁移、effect/Schema runtime 校验（用 Go struct tag 替代）、文件 watcher / 热重载、Auth / Account / Npm 依赖。
- **向前兼容** —— `Config` 结构体加新字段不影响 `mergeConfigs`，因为合并规则按字段类型分（scalar / slice / map），加 `Compaction CompactionConfig` 类的字段只需要写一行新的 override 逻辑。s09 的 `Agents []AgentConfig` 已经在 schema 里 —— 它会读 `cfg.Agents` 然后构造 runtime `Agent`。s10 / s11 / s12 / s13 同理。

opencode config 层的阅读顺序：

1. `packages/opencode/src/config/config.ts` L49-L110 —— merge 函数 + normalize（s08 mirror 的核心，本节正文）
2. `packages/opencode/src/config/config.ts` L120-L292 —— `Info` schema 全字段定义（s08 取 7 个字段子集）
3. `packages/opencode/src/config/paths.ts` —— `directories` / `files` walk-up 搜索（s08 在 `paths.go` mirror）
4. `packages/opencode/src/config/config.ts` L376-L450 —— `loadConfig` / `loadFile` / `loadGlobal` 完整 pipeline（s08 在 `Load` mirror）
5. `packages/opencode/src/config/parse.ts` —— JSONC 解析 + Schema 校验（我们用 stdlib `encoding/json` + struct tag 替代）
