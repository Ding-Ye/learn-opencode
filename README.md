# learn-opencode

> 用 Go 从零渐进构建一个 [sst/opencode](https://github.com/sst/opencode) mini 版。每一节加一个机制，每节末尾对照上游 TypeScript 源码。

[English](./README.en.md) · 中文

opencode 是一个 159K star 的开源 AI 编码 agent（competitor of Claude Code），底层是 TypeScript / Bun，monorepo 18 个包，核心 CLI 包 18 万行代码。直接读它会淹死。本仓库把它拆成 14 节，每节用 200-600 行 Go 重写一个机制，让你从能调通"一次 LLM 调用"开始，渐进长出 tool registry / permission / streaming loop / session 持久化 / agent registry / MCP / LSP / cost 追踪……

## Quickstart

```bash
git clone https://github.com/Ding-Ye/learn-opencode
cd learn-opencode/agents/s01-minimum-loop

export ANTHROPIC_API_KEY=sk-ant-...
go run . hello in three words

# 离线测试（用 httptest stub Anthropic）
go test ./...
```

跑 Web doc viewer：

```bash
cd web
npm install
npm run dev
# open http://localhost:3000/zh
```

## Curriculum

| # | slug | 标题 | mechanism |
|---|---|---|---|
| ✅ s01 | minimum-loop | 最小 agent loop | one-shot Anthropic call + print |
| ✅ s02 | message-parts | 消息与 Part 模型 | Part union (Text/Tool/File/Reasoning) |
| ✅ s03 | tool-registry | 工具注册表 | Tool interface + JSON schema + dispatch |
| ✅ s04 | permission-eval | 权限求值 | wildcard-match ruleset, last-match-wins |
| ✅ s05 | provider-iface | Provider 抽象 | Anthropic-only Provider interface |
| ✅ s06 | streaming-loop | 流式循环 | streaming text + tool_use parse |
| ✅ s07 | session-store | 会话存储 | SQLite Session/Message/Part tables |
| ✅ s08 | config-load | 配置加载 | hierarchical opencode.json merge |
| ⏳ s09 | agent-registry | Agent 注册表 | Agent.Info + permission cascade |
| ⏳ s10 | tool-loop | 工具执行循环 | streaming + dispatch + result feedback |
| ⏳ s11 | skills | 技能发现 | SKILL.md frontmatter scan |
| ⏳ s12 | mcp-client | MCP 客户端 | spawn child + JSONRPC over stdio |
| ⏳ s13 | lsp-client | LSP 客户端 | language-server stdio + workspace symbols |
| ⏳ s14 | cost-and-recovery | 成本与错误恢复 | token counting + retry classification |
| ⏳ s_full | integration | 端到端集成 | (doc only) |
| ⏳ App. A | provider-philosophy | 附录 A · Provider 抽象哲学 | mental model |
| ⏳ App. B | upstream-map | 附录 B · 上游源码导读地图 | reference |

✅ = doc 已发布；⏳ = 已规划，待发布。

## How to read

1. **clone 到本地**（Web doc viewer 需要 `docs/` 目录在仓库根）
2. **打开对应章节** `docs/zh/sNN-<slug>.md`（中文）或 `docs/en/sNN-<slug>.md`（English）
3. **跑代码** `cd agents/sNN-<slug> && go test ./...`
4. **读上游** 章节末尾「Upstream Source Reading」段会指出 opencode 的对应 `packages/opencode/src/...` 文件 + 行号

## Stack

- **Go 1.22+** — 教学骨架。每节一个独立 module（通过 `go.work`），无跨节 import。
- **Next.js 15 + Tailwind 4** — Web doc viewer，渲染 `docs/{zh,en}/*.md`。
- **GitHub Actions** — go matrix（每节 vet/build/test）+ web typecheck/build + docs zh-en parity。

## 致谢

- 上游：[sst/opencode](https://github.com/sst/opencode) (MIT, © 2025 opencode)
- 教学法启发：[shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)
- 生成工具：[learn-repo-generator skill](https://github.com/anthropics/claude-code)（Claude Code skill）

## License

MIT — see [LICENSE](./LICENSE).
