---
title: "附录 B · 上游源码导读地图"
chapter: 101
slug: appendix-b-upstream-map
est_read_min: 22
---

# 附录 B · 上游源码导读地图

> 本附录教什么：把 learn-opencode 当作地图来导航 `packages/opencode/src/` 那 18 万行 TS 代码。给你 4 条阅读路径；30+ 行的「上游文件 → 学习章节」对照表，方便你打开一个上游文件想知道学习版哪一节讲它；30 个跨 12 节的扩展练习，让你把教学代码长得更野；18 个 monorepo 子包的 in-scope / out-of-scope 切分；以及 7 个值得了解的相关项目。把这一节当作参考手册用，不是顺序读完的章节。

---

## 阅读顺序

opencode 太大，从顶往下读会淹死。根据你想做什么，从下面 4 条路径中挑一条。

**路径 (a) — 按特性复杂度逐层加深**

从最小的机制开始，一次只加一个。这条线和 learn-opencode 课程顺序一致，但用上游坐标表达：

1. `packages/opencode/src/permission/evaluate.ts`（约 50 行）—— 纯函数，无依赖。读通配符、`findLast` 语义、三种 action。
2. `packages/opencode/src/session/message-v2.ts`（150 行 part 定义）—— 数据模型。略读 Zod schema；关键是 Part union 的形状。
3. `packages/opencode/src/tool/tool.ts` + `packages/opencode/src/tool/registry.ts` —— Tool 接口（类比 Go interface）和按名分发的注册表。
4. `packages/opencode/src/provider/provider.ts`（1792 行，但目前只看 BUNDLED_PROVIDERS map 和 `model()` 函数）。
5. `packages/opencode/src/session/llm.ts` —— `streamText` 调用点。orchestration loop 在这里的精简形态。
6. `packages/opencode/src/session/session.ts` + `session.sql.ts` —— 持久化层。
7. `packages/opencode/src/agent/agent.ts` —— agent 定义和 permission 级联。
8. `packages/opencode/src/session/processor.ts` —— 完整工具执行循环 + snapshot + doom-loop 检测 + compaction 信号。
9. `packages/opencode/src/lsp/lsp.ts` 和 `packages/opencode/src/llm/protocols/mcp.ts` —— 两个 stdio-RPC 集成。

**路径 (b) — 按用户态执行流**

跟踪用户在 shell 里敲 `opencode "..."` 之后发生了什么：

1. `packages/opencode/bin/opencode`（Node 包装器）。
2. `packages/opencode/src/index.ts`（Yargs 入口）。
3. `packages/opencode/src/cli/cmd/run.ts`（RunCommand）。
4. `packages/opencode/src/cli/cmd/run/runtime.ts`（输入处理 + Session 创建）。
5. `packages/opencode/src/v2/session.ts`（`Session.Service.create` / `prompt`）。
6. `packages/opencode/src/agent/agent.ts`（`Agent.Service.get`）。
7. `packages/opencode/src/session/llm.ts`（`streamText` 调用）。
8. `packages/opencode/src/session/processor.ts`（流式处理 + 工具分发循环）。
9. `packages/opencode/src/permission/index.ts`（按工具的 gate）。
10. `packages/opencode/src/cli/cmd/run/scrollback.surface.ts`（终端渲染）。
11. `packages/opencode/src/cli/cmd/run/footer.permission.tsx`（交互式权限提示）。

**路径 (c) — 按 package**

`packages/opencode/src/` 树有 ~30 个子 package。按依赖顺序的 load-bearing 那些：

`config/` → `auth/` → `provider/` → `tool/` → `permission/` → `agent/` → `session/`（+`storage/`）→ `cli/cmd/run/` → `mcp/` 和 `lsp/`

正交的：`bus/`（事件总线）、`effect/`（DI 机器）、`plugin/`（扩展钩子）、`skill/`（SKILL.md 发现）、`snapshot/`（git-diff 回滚）。

**路径 (d) — 只有 1 小时**

按这个顺序读这 3 个文件，别的别看：

1. `packages/opencode/src/permission/evaluate.ts`（10 分钟）—— 给你 opencode 对工具调用说「不」的全部心智模型。
2. `packages/opencode/src/session/processor.ts` L34–L150（30 分钟）—— 真正的 agent loop。读到你能在餐巾纸上画出来：拉 stream → 遇 tool_use 就 gate → 执行 → 再调一次。
3. `packages/opencode/src/agent/agent.ts` L28–L304（20 分钟）—— Agent 是什么，permission 怎么 merge，"build" / "plan" / "general" 为什么不一样。

读完这一小时，你能不打哈哈地回答「opencode 是什么」。剩下都是实现选择或打磨。

## 按上游文件查对应章节

打开一个上游文件想知道学习版哪一节讲它的机制时，查下表。按上游路径排序。≥ 30 行。

| 上游文件 | 章节 | 何时第一次读它 |
|---|---|---|
| `packages/opencode/bin/opencode` | （无 —— bootstrap shell 脚本） | 想看 Node 包装器怎么挑平台二进制时。 |
| `packages/opencode/package.json` L74–L169 | （无 —— 依赖清单） | Quickstart 之后；告诉你哪些 AI SDK provider 包被 bundle。 |
| `packages/opencode/src/index.ts` L58–L120 | s01（CLI 入口类比） | 跟踪启动路径时；看 `Global.Path` 设置。 |
| `packages/opencode/src/cli/cmd/run.ts` L1–L40 | s10 | 想看用户输入 → orchestrator 交接时。 |
| `packages/opencode/src/cli/cmd/run/runtime.ts` | s07（Session create）、s10 | 想看 stdin/管道输入处理 + Session bootstrap 时。 |
| `packages/opencode/src/cli/cmd/run/demo.ts` | （out of scope） | 想不带 API key 研究代码时 —— 合成事件源。 |
| `packages/opencode/src/cli/cmd/run/scrollback.surface.ts` | （out of scope —— TUI） | 哪天你想做 Go TUI 移植时。 |
| `packages/opencode/src/cli/cmd/run/footer.permission.tsx` | s04（提示 UI 这边；评估逻辑共享） | 研究 human-in-the-loop 那侧的 permission ask 时。 |
| `packages/opencode/src/cli/cmd/run/stream.ts` | s06 + s10（事件 reduce → 状态机） | 想看 SSE 事件怎么变成 UI 状态时。 |
| `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 | 路径 (d) 第 1 项 —— 先读这个。 |
| `packages/opencode/src/permission/index.ts` L14–L29 | s04 + s10（ask 事件 emission） | s04 之后，想看 "ask" action 怎么传到用户面前时。 |
| `packages/opencode/src/config/config.ts` L49–L110 | s08 | 读 s08 时；opencode 的完整 schema 在这。 |
| `packages/opencode/src/config/paths.ts` | s08 | 紧跟 `config.ts`；展示 `.opencode/` 的向上 walk。 |
| `packages/opencode/src/config/mcp.ts` | s12（TODO —— 后续 release） | 现在略读；s12 上线再回来。 |
| `packages/opencode/src/auth/index.ts` | （out of scope） | 想加非 API-key 的 provider（OAuth、Bedrock IAM）时。 |
| `packages/opencode/src/provider/provider.ts` L87–L150 | s05、附录 A | s05 之后；这是 BUNDLED_PROVIDERS map。 |
| `packages/opencode/src/provider/provider.ts`（剩下 1792 行的部分） | （参考） | 调试某个 provider 集成时。 |
| `packages/opencode/src/session/message-v2.ts` L1–L150 | s02 | s02 紧跟着读 opencode 的完整 Part union。 |
| `packages/opencode/src/session/session.ts` L1–L110 | s07 | s07 之后看完整 schema。 |
| `packages/opencode/src/session/session.ts` L91–L142 | s14 | 研究 cost 追踪时。 |
| `packages/opencode/src/session/session.sql.ts` | s07 | 跟 `session.ts` 并排看 SQL 定义。 |
| `packages/opencode/src/session/llm.ts` L35–L120 | s01（一次性）→ s05（provider）→ s06（streaming） | s01 时第一次读；s06 后回头读完整流式流程。 |
| `packages/opencode/src/session/llm.ts` L100–L200 | s06 | SSE 消费侧；s06 之后读。 |
| `packages/opencode/src/session/processor.ts` L34–L150 | s10 | 路径 (d) 第 2 项 —— 第二个读。 |
| `packages/opencode/src/session/processor.ts` L734–L802 | s10（max-iterations / doom-loop） | s10 之后想看安全网时。 |
| `packages/opencode/src/session/message-error.ts` | s14 | 研究错误分类时。 |
| `packages/opencode/src/tool/registry.ts` L56–L150 | s03 | s03 之后看完整工具目录。 |
| `packages/opencode/src/tool/tool.ts` L35–L77 | s03 | 跟 registry 并排；定义 Tool 接口。 |
| `packages/opencode/src/tool/edit.ts`（和其它内置工具） | s03（只看形状 —— 真正的 `edit` ~600 行） | 实现你自己真正的 edit 工具时。 |
| `packages/opencode/src/agent/agent.ts` L28–L70 | s09 | s08 之后读；built-in agents 和它们的解析。 |
| `packages/opencode/src/agent/agent.ts` L128–L304 | s09 | permission 级联；agent.ts 最微妙的部分。 |
| `packages/opencode/src/skill/index.ts` L36–L150 | s11 | s11 之后看完整发现流程（含缓存）。 |
| `packages/opencode/src/lsp/lsp.ts` L17–L100 | s13（TODO —— 后续 release） | 现在略读；s13 上线再回来。 |
| `packages/opencode/src/llm/protocols/mcp.ts` | s12（TODO —— 后续 release） | 现在略读；s12 上线再回来。 |
| `packages/opencode/src/storage/db.bun.ts` | s07（我们用 `modernc.org/sqlite` 替代了 Bun 特定写法） | 想看 opencode 的 drizzle-orm 用法时。 |
| `packages/opencode/src/plugin/index.ts` L59–L150 | （out of scope） | 想给自己 fork 加外部插件时。 |
| `packages/opencode/src/snapshot/` | （out of scope） | 想给 learn-opencode 加工具失败回滚时。 |
| `packages/opencode/src/v2/session.ts` L68–L80 | s07 + s10（v2 客户端 API；学习版直接内嵌相同形状） | 研究多进程 opencode 时。 |

## 扩展练习

learn-opencode 给了你骨架；这些练习长肌肉。挑跟你想做下一步匹配的来。

### s01 扩展：

- 把请求体改成流式（`stream: true`）然后不用 s06 的框架自己消 SSE —— 就为亲手感受 wire 格式。
- 加 `--model` flag 让你能在 `claude-3-5-sonnet` 和 `claude-3-5-haiku` 之间切；观察成本差异。
- 加 `--system` flag 从文件加载；看长 system prompt 怎么影响延迟。

### s02 扩展：

- 加一个 `Part.Marshal` / `Part.Unmarshal` benchmark；Go 里 tagged union 不是免费的，量一下。
- 加 `PartImage` 变体（Anthropic 支持图片输入）；roundtrip 一个 base64 编码的 PNG。
- 给 `Part` 写 `String()` 方法，渲染成终端友好的格式（彩色文本 vs `[tool: read]` vs `[result: 14 lines]`）。

### s03 扩展：

- 实现真正的 `tree` 工具，用 `os.ReadDir` 走到可配置深度；注意 `JSONSchema()` 里 `max_depth: integer`。
- 加 `Registry.Disable(name string)` 方法；接到 config-driven 的「工具黑名单」上。
- 实现 `RegistryCatalog()` 函数，emit 一个 markdown 工具表 —— `--list-tools` CLI 用得上。

### s04 扩展：

- 加一个 `permission deny:bash:rm -rf*` 的测试用例；展示「后写的 deny 覆盖前面的 allow」。
- 加 CIDR 风格的模式（`edit:src/**`），通过包一层 `filepath.Match` 加 `**` 展开。
- 加 `Permission.Explain(target string)`，walk ruleset 告诉你哪条规则匹中（可审计的权限）。

### s05 扩展：

- 实现 `OpenAIProvider`（Anthropic 是简单那个 —— OpenAI 的 SSE framing 和 tool 形状略不同；都翻译成 canonical Event）。
- 加 `BedrockProvider`，用 AWS sigv4 签请求。
- 加 `RecordingProvider`，包另一个 Provider 把每次（请求、响应）写到 JSONL 文件用于回放测试。

### s06 扩展：

- 加 `--debug-events` flag，把每个解析出来的 Event dump 到 stderr；理解任何新 provider 流式格式时极好用。
- 加 `Loop.OnEvent(func(Event))` 回调让 TUI 增量渲染。
- 流中间随机注入网络错误，验证错误分类（s14）端到端还能 work。

### s07 扩展：

- 用 SQLite FTS5 加 `db.SearchMessages(query string)`；opencode 没出这个，~100 行教学代码。
- 把 schema 从 `mattn/go-sqlite3` 迁到 `modernc.org/sqlite`（纯 Go）；比较二进制大小和 cgo 约束。
- 加 `db.Vacuum()` 和 `db.Backup(path)` 提升生产 readiness。

### s08 扩展：

- 加 JSONC 支持（`//` 和 `/* */` 注释），写个注释 stripper 跑在 `encoding/json` 之前；opencode 用第三方库做这个。
- 加 `OPENCODE_CONFIG_OVERRIDE` env var 接收 JSON 字符串，最后 merge；CI 用得上。
- 加 config schema 校验：当 `provider.modelID` 缺失时 emit 友好错误指出问题行。

### s09 扩展：

- 实现自定义「researcher」agent：只读权限、模型默认 opus、system prompt 偏向引用源。
- 加 `AgentRegistry.List(mode Mode)` 过滤器，让 CLI 只列 `ModePrimary` agent。
- 加 agent 级 `Tools []string` 白名单强制（目前只有 permission 强制；tool 列表是 advisory）。

### s10 扩展：

- 加 `--max-iterations` flag 和测试，验证 cap 真的会触发（不是只走过场）。
- 加 `--dry-run` 模式：把每个工具都过 `ask`，永不执行；预览 agent 会做什么时有用。
- 补上缺失的 `compaction` 步：当 s14 信号 overflow 时，把前一半消息总结成单条合成 system 消息再重试。

### s11 扩展：

- 加 per-skill capability 声明（`required_tools: [edit, bash]`），加载时若当前 agent 不放行就警告。
- 加 `SKILL.md` linter 抓缺失的 frontmatter 字段。
- 加 hot reload：每次 `Run()` 调用都重扫 skills 目录（很便宜；50 个 skill 大约 5ms）。

### s14 扩展：

- 实现 `ContextOverflowError` 信号实际触发的 compaction 例程 —— 用一个旁路 LLM 调用 summarize 0..N-10 消息。
- 加 `pricing.go`，启动时从 JSON 文件加载费率而不是硬编码常量。
- 加 `--budget-cents` CLI flag，当 `Usage.TotalCost(p) * 100 > budget` 时中断 loop。

## opencode 的 18 个 package

opencode 是 monorepo，所有 package 在 `packages/` 下。in-scope / out-of-scope 的切分反映「这是在教 agent loop，还是在教它周边的基础设施？」

| Package | Scope | 一句话理由 |
|---|---|---|
| `packages/opencode/` | **in scope** | 核心 CLI；s01..s14 重新实现的所有东西都在这。 |
| `packages/sdk/` | out of scope | TypeScript 客户端 for opencode HTTP server；学习版是单进程。 |
| `packages/plugin/` | out of scope | npm 分发的插件 SDK；Go 用 build-tag 插件代替。 |
| `packages/console/` | out of scope | hosted opencode 的 web 控制台；本地 agent 用不上。 |
| `packages/web/` | out of scope | Marketing 站点（也不是 doc viewer）。 |
| `packages/docs/` | out of scope | Astro 构建的 docs 站点（跟 learn-opencode 的 `docs/` 分开）。 |
| `packages/desktop/` | out of scope | Electron 包装器；用同一个 CLI。 |
| `packages/app/` | out of scope | Mobile/PWA 前端，对 opencode ACP server 说话。 |
| `packages/extensions/` | out of scope | 编辑器扩展（VS Code、JetBrains）。 |
| `packages/storybook/` | out of scope | UI 组件 playground。 |
| `packages/ui/` | out of scope | desktop/web/console 共享 UI primitives。 |
| `packages/identity/` | out of scope | hosted opencode 的 OAuth identity provider。 |
| `packages/share/` | out of scope | 通过签名 URL 分享（s_full omissions 表里提到）。 |
| `packages/slack/` | out of scope | hosted opencode 的 Slack 集成。 |
| `packages/script/` | out of scope | 构建/发布脚本；不是 runtime 部分。 |
| `packages/http-recorder/` | out of scope | 测试 fixture 录制；学习版用 `httptest` 代替。 |
| `packages/containers/` | out of scope | hosted opencode 的 Docker manifest。 |
| `packages/enterprise/` | out of scope | License-gated 企业特性。 |

17 个 out-of-scope 包加起来 200K+ LOC。决定忽略它们是 learn-opencode 能在一个周末读完的最大原因。

## 相关阅读

opencode 的 6 个邻居，值得了解。它们都不是依赖；读它们给你 opencode 的设计选择和替代方案上下文。

| 项目 | 为什么有意思 |
|---|---|
| **Claude Code**（Anthropic 官方 CLI） | 最近的类比。闭源，但公开 doc 和 [Claude Code SDK](https://docs.anthropic.com/en/docs/claude-code/sdk) 描述了类似的 agent loop（tools / permissions / sessions）。比较 *用户 surface*；opencode 是种重实现哲学（"如果 Claude Code 闭源，工作流能否存活？"）。 |
| **sst/sst**（同一作者的 infra-as-code） | opencode 在 SST org 里。Effect-based DI、local-first 状态、用 Bun 构建 npm monorepo —— 全 SST 风格。先读 SST 解释为什么 opencode 长这样。 |
| **Vercel AI SDK**（npm 上的 `ai`） | opencode 包的库。它的接口设计课（LanguageModelV3、providerOptions、tool 格式 normalization）是附录 A 的基础。即使你从不写 TS，这个设计是可移植的。 |
| **Continue**（continue.dev） | 一个开源 IDE 扩展 agent。surface 不同（编辑器内 sidebar，不是 CLI），但 loop 一样。它的 `Tool` 模型和 `provider` 适配器是有用的对照；它在生产里 ship MCP 集成。 |
| **Cursor**（闭源，但可观察） | opencode 对标的产品。下了不同的注：深度集成编辑器 vs 终端原生。看 Cursor 工具集合演进（search → grep → edit → background agents）是任何 agent 的路线图预览。 |
| **Aider**（paul-gauthier/aider） | 一个早于当前一波的 Python agent。surface 更小，没有权限系统、没有 MCP —— 但是「最小可行编码 agent」的清晰参考。在 learn-opencode 之后读它，能看清 opencode 加了什么。 |
| **Anthropic 的 `mcp` 参考实现**（[modelcontextprotocol/servers](https://github.com/modelcontextprotocol/servers)） | opencode 会跟它说话的 MCP 服务器。s12 上线后，你会在那里找到示例服务器（filesystem、git、postgres 等）拿来测试，不用自己起。 |
