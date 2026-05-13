---
title: "s_full · 端到端集成"
chapter: 99
slug: s_full-integration
est_read_min: 18
---

# s_full · 端到端集成

> 本章教什么：s_full 没有任何新代码。它用到的每一个机制在 s01..s14 里都已经造好了（s12 MCP 与 s13 LSP 标记为 TODO — 后续 release 补齐）。本章新加的是**接线**：12 个小模块如何拼成一个完整 agent，把 `opencode "add error handling to app.ts"` 这样一行 CLI 字符串变成「写进 SQLite + 算清成本 + 经过权限闸门 + 多轮 tool 调用」的真实对话。想要鸟瞰图看本章；想要每个盒子怎么造的，回去看 s01..s14。

---

## 大架构图

12 个模块，每根箭头代表一条数据依赖。从上到下、从左到右读；只有持久化时箭头才向上回弯。

```
                 +----------------+
                 |     CLI argv   |   "opencode 'add error handling to app.ts'"
                 +--------+-------+
                          |
                          v
   +----------+    +--------------+     +------------+    +-----------+
   |  Config  |--->| AgentRegistry|<----| Skills cat.|    |  Session  |
   |   s08    |    |     s09      |     |    s11     |    |    s07    |
   +----------+    +------+-------+     +------+-----+    +-----+-----+
       JSON 合并        |                     |                ^
                         v                     v                |
                  +--------------+     system prompt       每条 Message
                  | Orchestrator |<----+                   + Part 都落库
                  |     s10      |     |                        |
                  +------+-------+     |                        |
                         |             |                        |
              Request    v       并入 skill catalog              |
                  +--------------+                              |
                  |   Provider   |   Stream(ctx, Request)       |
                  |     s05      |---+                          |
                  +------+-------+   |                          |
                         | SSE       v                          |
                         |     +-----------+                    |
                         +---->| Streaming |   Event chan       |
                               | Loop  s06 |---+                |
                               +-----------+   | parts          |
                                               v                |
                            +--------+   +----------+           |
                tool_use -->| Permis |-->| ToolReg  |--exec---->|
                            | s04    |   |   s03    |           |
                            +--------+   +----+-----+           |
                                              | result          |
                                              v                 |
                                         +----------+           |
                                         |  Usage   |           |
                                         |   s14    |---累加--->|
                                         +----------+           |
                                                                |
                  +-----------+        429 → backoff            |
                  |  Retry    |<-------auth/overflow 分类-------+
                  |   s14     |
                  +-----------+
```

图想说 / 不想说什么：

- **实线箭头 = 函数调用**。Config 启动时喂给 AgentRegistry 一次；AgentRegistry 喂给 Orchestrator 一个具名 Agent（带 model、system prompt、permission ruleset）。Orchestrator 驱动 Provider；Provider 把 SSE 流回 Streaming Loop；Loop 把 PartToolUse 抛给 Tool Registry，途中由 Permission 把闸。每一轮往返都写到 Session 库、累加到 Usage。
- **图里没有 LLM 自己**——它在 Provider 那根线另一头的云上。从 learn-opencode 的角度，Anthropic Messages API 就是一个 HTTP POST + 一个 SSE response，仅此而已。
- **图里没有 s01 / s02**——这俩是引导章，代码已被吸收：s01 的「打一次 HTTP」住在 s05 的 `AnthropicProvider` 里；s02 的 `Part`/`Message` 类型每节都自带一份（每节都是独立 Go module，没有跨节 import）。
- **MCP (s12) 与 LSP (s13)** 会作为远端工具挂进 Tool Registry。它们是 TODO — 后续 release 补齐；s03 的 `Tool` interface 已经按照能吸收远端 tool 的形状设计好了，吸收时 orchestrator 不用动。

## 端到端用例

`research-notes.md` A3 那个 16 步 trace，重新讲一遍，每步同时给上游和 learn-opencode 引用。

用户调用：

```bash
opencode "add error handling to app.ts"
```

下方 16 步的路径相对 `.learn/upstream/`。每步同时点出再造它的 learn-opencode 章节；「—」表示这一步是 TUI / 包装层，learn-opencode 有意省略。

| # | 发生了什么 | 上游文件 | learn-opencode |
|---|---|---|---|
| 1 | Node wrapper 探测平台、找到预编译 binary、spawn 它。 | `packages/opencode/bin/opencode` | —（Go 单 binary） |
| 2 | Yargs 解析 argv；加载 `Global.Path`；设置 `OPENCODE_PID`。 | `packages/opencode/src/index.ts` L58–L120 | s01 `main.go`（单文件 CLI） |
| 3 | `RunCommand` dispatch 把任务交给 runtime。 | `packages/opencode/src/cli/cmd/run.ts` L1–L40 | s10 `Orchestrator.Run` 入口 |
| 4 | Runtime 解析 stdin / 管道输入；实例化 `OpencodeClient`；创建 Session。 | `packages/opencode/src/cli/cmd/run/runtime.ts` | s07 `OpenDB` + `CreateSession` |
| 5 | `Session.Service.create()` 分配 SessionID、写入 SessionTable。 | `packages/opencode/src/v2/session.ts` L68–L75 | s07 `db.go`（`Session` / `Message` / `Part` 三张表） |
| 6 | 提交 prompt：`client.session.prompt({ text, model, agent: "build" })`。 | （同 #5 文件） | s10 `Orchestrator.Run(ctx, Message{Role: User})` |
| 7 | Agent 查找：`Agent.Service.get("build")` 合并 permissions、选 model。 | `packages/opencode/src/agent/agent.ts` L58–L70 | s09 `AgentRegistry.Resolve("build")` |
| 8 | Config (s08) 启动时已加载；agent permissions 是 defaults→config→agent override 的级联。 | `packages/opencode/src/config/config.ts` L49–L110 + `agent.ts` L128–L304 | s08 `Config.Load` + s09 `mergePermissions` |
| 9 | 扫描 `.opencode/skills/` 与 `~/.opencode/skills/`；catalog 字符串拼进 system prompt。 | `packages/opencode/src/skill/index.ts` L36–L150 | s11 `Discover` + `Catalog.String()` |
| 10 | LLM 调用：`streamText({ model, system, messages, tools })`。SSE response 开始。 | `packages/opencode/src/session/llm.ts` L35–L120 | s05 `AnthropicProvider.Stream(ctx, Request)` |
| 11 | Streaming loop 消费 `content_block_delta` / `tool_use_start` / `message_stop` 事件，攒 Parts。 | `packages/opencode/src/session/llm.ts` L100–L200 | s06 `Loop.Consume(stream)` |
| 12 | 第一个 tool_use 到来（如 `read("app.ts")`）。Permission 评估器把 `read:app.ts` 拿到合并后的 ruleset 上跑 → `allow`。 | `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 `Evaluate(perm, target, rulesets...)` |
| 13 | Tool dispatch：`Registry.Dispatch("read", input)` 返回文件内容；结果变成一个 `PartToolResult`。 | `packages/opencode/src/tool/registry.ts` L56–L150 + `tool/tool.ts` L35–L77 | s03 `Registry.Dispatch` + s10 result feedback |
| 14 | Orchestrator 把所有 results 打包成一条 user Message，再调一次 Provider。直到 assistant 给出 `end_turn`（或撞到 `MaxIterations`）。 | `packages/opencode/src/session/processor.ts` L34–L150 | s10 `Orchestrator.Run` 主循环 |
| 15 | 每次请求结束，`Usage{InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens}` 累加到 SessionRow。错误流过 retry 分类器（429 backoff、401 直接退出、context overflow → compaction 信号）。 | `packages/opencode/src/session/session.ts` L91–L142 + `session/message-error.ts` | s14 `Usage.Add` + `WithRetry` + 四种错误类型 |
| 16 | Idle timeout 触发；冲刷剩余 messages；关 SQLite；exit 0。 | `packages/opencode/src/cli/cmd/run.ts` finalize | s07 `db.Close()`（defer） |

trace 想点出几件事：

- **整个仓库只有一个 agent loop** —— 它住在 s10 的 `Orchestrator.Run` 里。第 10–14 步全部发生在一个 Go 函数体内，s05/s06 的 SSE 流喂出 tool call 给 s03、由 s04 把闸、result 进入下一次 request body。
- **两条侧通道不在 loop 主体里**：持久化（每条 message+part 同时落到 s07 的 SQLite）、用量记账（每个 `EventFinish` 累加到 s14 的 `Usage`）。
- **一条没画出来的范围扩张**：当对话超过 ~120k tokens，Provider 返回 `context_length_exceeded`，s14 的分类器返回 `ShouldCompact == true`，orchestrator 应当调用 compaction 例程（s14 不实现真正的 compaction —— 信号已发出，实现留作练习）。

## 模块对照表

12 行，每行一对 上游 → learn-opencode 映射。打开一个上游文件想找对应 Go，看这个表。

| 上游文件 | 章节 | learn-opencode 关键文件 |
|---|---|---|
| `packages/opencode/src/session/llm.ts` L35–L120 | s01 `minimum-loop` | `agents/s01-minimum-loop/main.go`、`provider.go` |
| `packages/opencode/src/session/message-v2.ts` L1–L150 | s02 `message-parts` | `agents/s02-message-parts/parts.go` |
| `packages/opencode/src/tool/registry.ts` L56–L150 + `tool/tool.ts` L35–L77 | s03 `tool-registry` | `agents/s03-tool-registry/tool.go`、`builtin_*.go` |
| `packages/opencode/src/permission/evaluate.ts` L9–L50 | s04 `permission-eval` | `agents/s04-permission-eval/permission.go` |
| `packages/opencode/src/provider/provider.ts` L87–L150 | s05 `provider-iface` | `agents/s05-provider-iface/provider.go`、`stream.go` |
| `packages/opencode/src/session/llm.ts` L100–L200（SSE 帧） | s06 `streaming-loop` | `agents/s06-streaming-loop/sse.go`、`loop.go`、`fake_provider.go` |
| `packages/opencode/src/session/session.ts` L1–L110 + `session.sql.ts` | s07 `session-store` | `agents/s07-session-store/db.go`、`schema.go` |
| `packages/opencode/src/config/config.ts` L49–L110 + `config/paths.ts` | s08 `config-load` | `agents/s08-config-load/config.go`、`paths.go` |
| `packages/opencode/src/agent/agent.ts` L28–L304 | s09 `agent-registry` | `agents/s09-agent-registry/agent.go`、`builtin_agents.go` |
| `packages/opencode/src/session/processor.ts` L34–L150 | s10 `tool-loop` | `agents/s10-tool-loop/loop.go` |
| `packages/opencode/src/skill/index.ts` L36–L150 | s11 `skills` | `agents/s11-skills/skill.go` |
| `packages/opencode/src/session/session.ts` L91–L142 + `session/message-error.ts` | s14 `cost-and-recovery` | `agents/s14-cost-and-recovery/usage.go`、`retry.go`、`compaction.go` |

两个 TODO — 后续 release 补齐：

- `packages/opencode/src/config/mcp.ts` + `packages/opencode/src/llm/protocols/mcp.ts` → s12 `mcp-client`（spawn child + JSON-RPC over stdio；远端 tool 注册进 s03 的 `Registry`）。
- `packages/opencode/src/lsp/lsp.ts` L17–L100 → s13 `lsp-client`（gopls 子进程 + workspace symbols；LSP 形状的 tool 注册进 s03 的 `Registry`）。

s12、s13 上线后，它们插在用例第 13 步（Tool dispatch），不需要改 orchestrator —— 这就是 s03 `Tool` interface 作为「能被 dispatch 的东西」单一来源的核心收益。

## 有意省略的部分

opencode 是一个 183K 行 TypeScript 应用。learn-opencode 是教学仓库，刻意控制在 6K 行以内。下表是我们没做的、为什么不做的；想深挖某一行，上游文件就是入口。

| 特性 | 为什么省略 | 上游文件（好奇心驱动） |
|---|---|---|
| TUI 渲染（OpenTUI / Solid.js、scrollback + footer 拆分） | 把 Solid 跑在终端、自带 reflow 和 prompt UI 的渲染器，本身就是几千行的独立项目；和 agent loop 无关。无头 CLI 已经够教 LLM 机制。 | `packages/opencode/src/cli/cmd/run/scrollback.surface.ts`、`packages/opencode/src/cli/cmd/run/footer.permission.tsx` |
| MCP client（spawn server、JSON-RPC handshake、远端 tool 注入） | TODO — 后续 release 作为 s12 补齐。s03 已经留好接口。 | `packages/opencode/src/config/mcp.ts`、`packages/opencode/src/llm/protocols/mcp.ts` |
| LSP client（workspace symbols、definition、hover） | TODO — 后续 release 作为 s13 补齐。和 s12 同一套 JSON-RPC 机制，方法名不同。 | `packages/opencode/src/lsp/lsp.ts` |
| Bun 专属 FFI（`Bun.$`、`Bun.file`、`Bun.SQLite`） | Bun 在 Go 教学仓里搬不动；用 `os/exec`、`os.ReadFile`、`mattn/go-sqlite3` 替代。 | `packages/opencode/src/storage/db.bun.ts` |
| Effect-based DI（`Layer`、`Context`、`Service`、`Effect.gen()`） | Go 用 constructor injection + `context.Context`；复刻 Effect 的 typed-error 机制只会遮蔽 agent 机制本身。 | `packages/opencode/src/effect/` |
| Drizzle ORM 加 migration runner | 幂等 `CREATE TABLE IF NOT EXISTS` 足够教学；生产 migration 需要一套独立故事我们没在教。 | `packages/opencode/src/storage/db.bun.ts`（drizzle-kit 用法） |
| 分享 / GitHub PR export | OAuth + Octokit + share API 是产品特性，不是机制。 | `packages/opencode/src/share/`、`packages/opencode/src/sync/` |
| 移动 / ACP server（远端 agent 控制协议） | agent loop 之上的第二种线协议；我们教的是 loop 本身。 | `packages/opencode/src/acp/` |
| Plugin loader 带 hot reload（chokidar 监听） | Go 用 build-tag plugin 或 constructor injection 覆盖同一片地，不需要 watcher 子系统。 | `packages/opencode/src/plugin/index.ts` L59–L150 |
| 多进程（CLI + headless server + SDK client） | Go 单进程 binary 更易读、易跑。 | `packages/opencode/src/cli/cmd/serve.ts`、`packages/opencode/src/v2/` |
| GitHub Copilot / Codex / Azure / DigitalOcean auth providers | 每个都是单点 OAuth 流；BUNDLED_PROVIDERS map（附录 A）才是机制，单条目不是。 | `packages/opencode/src/plugin/github-copilot/copilot.ts`、`packages/opencode/src/auth/` |
| Snapshot / 工具失败 git-diff 回滚 | 该进未来「safety nets」章节；不在核心 loop 范围。 | `packages/opencode/src/snapshot/` |

## 接下来读什么

四条阅读路径，按你来这里的目的选。

**路径 A —— 主干线（推荐顺序）**

按 s01 → s02 → s03 → s04 → s05 → s06 → s07 → s08 → s09 → s10 → s11 → s14 → s_full 读。每章假定前面都读过。这就是课程表设计的顺序：到 s10 你已经造完一个真 loop，s11 / s14 / s_full 是在它上面延伸。

**路径 B —— 主题切片**

只关心 opencode 的某一片：

- **LLM 传输**：s01（一次性 HTTP）→ s05（Provider 接口 + Anthropic）→ s06（流式 SSE）。
- **工具与安全**：s03（Registry）→ s04（Permissions）→ s10（loop 里的闸门 dispatch）→ 附录 A（为什么是 interface）。
- **持久化与记账**：s07（SQLite tables）→ s14（Usage + retry 分类器）。
- **面向用户的配置**：s08（opencode.json 层级）→ s09（Agent registry）→ s11（Skills）。

**路径 C —— 从 loop 开始读**

如果你读过 agent 论文、想直接看「loop」，从 **s10**（`agents/s10-tool-loop/loop.go`）入手。看到 `provider.Stream(ctx, req)` 就跳回 s06（消费 stream）和 s05（stream 来自哪里）。看到 `permission.Evaluate(...)` 就跳到 s04。看到 `registry.Dispatch(...)` 就跳到 s03。loop 大约 250 行；它依赖的所有东西都只隔一章。

**路径 D ——「agentic」线**

如果你专程为 agent 抽象（多 agent 系统、permission cascade、成本上限）而来，按 s09 → s10 → s14 读。s09 解释 Agent 是什么、它的 permission ruleset 来自三处合并；s10 展示这个 ruleset 在 loop 里如何把闸每一次 tool call；s14 展示 loop 的成本如何被观测、它的失败如何被分类以恢复。读完跳到附录 A 看「为什么是 interface 不是具体类型」的心智模型。

## 读完 learn-opencode 之后

12 章读完想继续：

- **造 s12 与 s13。** Interface 已经为它们留好。s12 需要 `os/exec` + 一小段 stdio JSON-RPC client + 一个实现 `Tool` 的 `MCPTool` 适配器。s13 是同一套机器跟 `gopls` 通信。各自约 600 / 500 行。
- **把无头 CLI 换成真正的 TUI。** 用 `bubbletea`/`tcell` 把 opencode 的 scrollback+footer 移植成 Go 版，是个健康的 2K-LOC 子项目，和 s01..s14 完全正交。
- **加一个第二 provider。** 附录 A 走过 `OpenAIProvider` 需要什么：同样的 `Provider` interface、不同的 request shape、不同的 SSE 帧。s05 interface 的核心收益就是 agent 其它部分一行不用改。
- **回去读上游。** 12 节都装进脑子后，opencode 那 183K 行核心 CLI 包就有迹可循了。附录 B 是阅读地图；下一个最值得读的文件是 `packages/opencode/src/session/processor.ts` —— s10 `Orchestrator.Run` 的完整版，带 snapshot-on-tool-call、doom-loop 检测、真正的 compaction 触发。
