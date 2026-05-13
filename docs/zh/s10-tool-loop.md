---
title: "s10 · 工具执行循环"
chapter: 10
slug: s10-tool-loop
est_read_min: 13
---

# s10 · 工具执行循环

> 本章教什么：把前 9 节的所有零件 —— Provider (s05/s06) + Tool Registry (s03) + Permission Evaluate (s04) + Message/Part (s02) —— 合成一个真正的 agent loop。`Orchestrator.Run(ctx, initial)` 拿着 LLM 和工具走一个 5 步循环：拉一次 Stream → 攒一个 assistant Message → 找出里面的所有 tool_use → 每个跑一次 (permission gate + tool execute) → 把所有结果打包成一个 user Message → 再喂回 LLM。直到 assistant 不再 tool_use（自然 end_turn）或撞到 `MaxIterations`。Mirror upstream `packages/opencode/src/session/processor.ts` L34-L150 + L734-L802。

---

## Problem

到 s09 为止，所有零件都做好了：能跟 LLM 通信（s05/s06），能调用工具（s03），能判断哪些工具允许（s04），能把消息分解成 Part（s02），能加载配置和 agent（s08/s09）。但**它们没拼起来过**。

具体来说：s06 的 `Loop.Consume` 拉完一个 stream 就停了 —— 它把 EventToolUse 攒成 PartToolUse，但没人去 *运行* 这个 tool。s03 的 `Registry.Dispatch` 能跑一个 tool，但它不知道该跑哪个 —— 没人去 *扫描* 上一轮 assistant Message 里的 tool_use 列表。s04 的 `Evaluate` 能判断某个 (permission, target) 是 allow / deny / ask，但它从不被自动调用 —— 没人在 tool 执行前 *拦* 一下。

更具体的问题是 **多轮**。一个真实的 agent 会话长这样：

```
user:      把 a.go 里所有 TODO 改成 FIXME
assistant: 好的，先 grep 找出所有 TODO  +  tool_use(grep, "TODO", "a.go")
[run grep, get back 3 matches]
user:      tool_result(grep): a.go:5: // TODO ...
                              a.go:12: // TODO ...
                              a.go:38: // TODO ...
assistant: 我用 edit 把它们逐个改  +  tool_use(edit, ...)  +  tool_use(edit, ...)  +  tool_use(edit, ...)
[run 3 edits]
user:      tool_result(edit#1): ok
           tool_result(edit#2): ok
           tool_result(edit#3): ok
assistant: 改完了，3 处 TODO 都换成了 FIXME。
```

LLM 一轮里只能根据"截止当前所知"做决策。第一轮它不知道 a.go 里 TODO 在哪 —— 必须先 grep；grep 结果回来，第二轮它才能决定怎么 edit；edit 结果回来，第三轮才能宣布 done。**没有一个 wrapper 把这三轮串起来，agent 就是个 chatbot**。

s10 的 `Orchestrator` 就是那个 wrapper。

## Solution

一个 struct + 一个方法：

```go
type Orchestrator struct {
    Provider      Provider
    Tools         *Registry
    Permissions   Ruleset    // 已 merge 的 cascade 结果（s09 的 MergePermissions 出来的）
    MaxIterations int        // 安全上限；0 = 无限（生产里会按 token budget 上限改）
}

func (o *Orchestrator) Run(ctx context.Context, initial []Message) ([]Message, error)
```

`Run` 内部就是那个 5 步循环：

```
for iter := 0; iter < MaxIterations; iter++ {
    (1) req := build Request from running []Message
    (2) stream := Provider.Stream(ctx, req)
        assistant := drain stream into one Message
        append assistant to trail
    (3) if assistant has no tool_use Parts → return trail (自然 end_turn)
    (4) for each tool_use in assistant.Parts:
          action := Permission.Evaluate(name, target, Permissions)
          if Deny  → tool_result{IsError: true, "permission denied: ..."}
          else     → out, err := Registry.Lookup(name).Execute(ctx, input)
                     err  → tool_result{IsError: true, err.Error()}
                     ok   → tool_result{IsError: false, out}
    (5) append user Message containing all tool_result Parts
}
return trail, ErrMaxIterationsExceeded  // 撞顶
```

| 步骤 | 用了哪节的成果 | 关键决定 |
|---|---|---|
| (1) build Request | s06 的 ProviderMessage 形状 | `messagesToProvider` 翻译 in-memory Parts → wire-shape ContentBlocks |
| (2) Stream + drain | s06 的 Loop.Consume 算法 | 复制进 `consumeOne`（每节自包含，无跨 import） |
| (3) end_turn 检测 | s06 的 `inferStopReason` | 简单：assistant Parts 里没有 PartToolUse → done |
| (4a) Permission gate | s04 的 Evaluate | last-match-wins；deny 不是 Run-level 错误，是 in-band 反馈 |
| (4b) Tool dispatch | s03 的 Registry.Lookup + Tool.Execute | err 也是 in-band；`runOneTool` 把三种 outcome 都拍平成 `*ToolResultPart` |
| (5) tool_results 打包 | s02 的 PartToolResult Part 类型 | **一个 user Message，多个 tool_result Part**（Anthropic 的 wire 约定） |

**两个 in-band 决定特别关键**：

- **Permission deny 是 in-band**：被拒的 tool 产生 `ToolResultPart{IsError:true, "permission denied"}`，下一轮 LLM 会看到这个 deny。LLM 自己决定怎么处理 —— 通常是结束 turn 或换种方式。Run **不返回**错误。
- **Tool error 也是 in-band**：`Tool.Execute` 返回 err 同样变成 `ToolResultPart{IsError:true, err.Error()}`。LLM 读自己的错，自我纠正。

Run 只在两种情况下返回非 nil err：(a) `Provider.Stream` 本身失败（transport-level），(b) 撞 MaxIterations。其他一切——deny、tool err、unknown tool——都是 in-band 信号给 LLM，loop 继续。

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│  s10 Orchestrator.Run 一轮迭代                                  │
│                                                                 │
│   trail = [user("...")]                                         │
│                                                                 │
│   ┌─ iter 0 ──────────────────────────────────────────────────┐ │
│   │ (1) req := messagesToProvider(trail) + toolSchemas()      │ │
│   │ (2) stream := Provider.Stream(ctx, req)                   │ │
│   │     → drain Events: text + text + tool_use(echo, "hi")    │ │
│   │     → assistant = Message{Parts: [text, tool_use]}        │ │
│   │     → append to trail                                     │ │
│   │ (3) collectToolUses(assistant.Parts) → [echo]             │ │
│   │     有 tool_use → 不退出                                  │ │
│   │ (4) for echo: Evaluate("echo", "{...}", Permissions)      │ │
│   │       → ActionAllow → tool.Execute → "hi"                 │ │
│   │       → tool_result{IsError:false, Content:"hi"}          │ │
│   │ (5) append user{Parts: [tool_result]}                     │ │
│   │     trail = [user, asst#0, user-result]                   │ │
│   └────────────────────────────────────────────────────────────┘ │
│                                                                 │
│   ┌─ iter 1 ──────────────────────────────────────────────────┐ │
│   │ (1) req := messagesToProvider(trail) ← 现在 3 条          │ │
│   │ (2) Provider.Stream → "echo returned hi. done."           │ │
│   │     → assistant = Message{Parts: [text]}                  │ │
│   │ (3) collectToolUses(...) → []                             │ │
│   │     ★ 没有 tool_use → return trail, nil                  │ │
│   └────────────────────────────────────────────────────────────┘ │
│                                                                 │
│   最终 trail = [user, asst#0, user-result, asst#1]               │
│   Provider.Stream 调用次数 = 2                                   │
└─────────────────────────────────────────────────────────────────┘
```

**五个 load-bearing 决定**：

1. **每轮一次 Provider.Stream，串行 drain 完再跑 tool**。Upstream 的 `processor.ts` 是边收 stream 边 dispatch tool（"tool-call" event 来一个跑一个，并发的）—— 我们简化成"先把整轮 assistant message 收完，再串行跑里面所有 tool"。简化的代价：assistant message 还在生成的时候 tool 不能开始跑（少了一点 latency overlap）；好处：`runOneTool` 是同步阻塞的 Go 函数，不需要 deferred / Promise / 通道。
2. **Tool 是串行跑的**。一个 assistant message 里有 3 个 tool_use 就一个一个跑，不并发。Upstream 也串行（每个 tool 的 snapshot/permission 握手天然是顺序的）。s10 之后的 ext-exercise 可以加 errgroup —— Orchestrator 的 contract 不变，只改 `runOneTool` 的调用循环。
3. **Permission deny 不是 Run-level 错误**。Run 返回 nil err、trail 完整、tool_result Part 上 IsError=true。LLM 看到自己被拒了，通常下一轮就 end_turn。这是 *agent 自我纠错* 的核心机制 —— 把 deny 当 transport error 一样冒泡上去就破坏了纠错回路。
4. **Tool error 同理**。`Tool.Execute` 返回 err → `tool_result{IsError:true, err.Error()}`。LLM 读自己的报错并修正。这条让 `DieTool` 这种永远失败的工具不会让 Run 报错（只是会让 LLM 看到一个 IsError 反馈）。
5. **MaxIterations 是简单上限**。生产 opencode 用 token / cost budget 卡（s14 的活），但永远要有一个上限 —— 无限的 LLM agent loop 是怎么醒来发现 $400 账单的方式。我们用最简单的迭代计数：iter==MaxIterations 时 return ErrMaxIterationsExceeded，trail 是"撞顶时的状态"，caller 可以用更高的 cap 重新 Run，picks up where it left off。

**为什么 ~600 LOC**：因为这是 *第一次* 4 个机制（Parts/Tool/Permission/Streaming）合在一起，每个都要在本 module 里 re-implement（按课程规则，每节自包含、零跨节 import）。`parts.go` 复制 s02、`provider.go` 复制 s06、`permission.go` 复制 s04、`tool.go` 复制 s03 —— 这就 ~400 行。`loop.go` 加 `Orchestrator` + `runOneTool` + `consumeOne` + `messagesToProvider` ~250 行。fakes + tools + 测试 ~200 行。合起来正好。

## What Changed (vs. s06)

s06 的 `Loop.Consume` 只做一件事：拉完一个 stream 就退出。s10 的 `Orchestrator.Run` 真正去 *invoke* tool 然后 *再回来*：

```diff
 // s06: 一次 stream，停。tool_use 只是个 Part，没人执行。
-loop := &Loop{Provider: provider}
-msg, err := loop.Consume(ctx, req)
-// msg.Parts 里可能有 PartToolUse —— 但 caller 自己想办法
-// (s06 的测试就停在这里 assert Parts 长这样)

+// s10: 多轮 loop，自动跑 tool 并回填结果。
+orch := &Orchestrator{
+    Provider:      provider,
+    Tools:         tools,
+    Permissions:   merged, // s09 的 cascade 结果
+    MaxIterations: 10,
+}
+trail, err := orch.Run(ctx, []Message{userMsg})
+// trail 里有完整的多轮历史：user → asst → user-result → asst → ...
+// 每个 PartToolUse 都被 *运行* 过、permission 被 *判过*、结果被 *回喂* 给 LLM 过。
```

`Loop.Consume` → `Orchestrator.consumeOne` 几乎是 1:1 复制（同样的 streaming assembly 算法 —— 相邻 text 合并、tool_use 断行、reasoning 同样合并）。区别是 `consumeOne` 是 *Run 的 helper*，不再是顶级 entry point —— 上面套了一层 for 循环。

**Permission 接口的演化**：s04 把 `Evaluate(perm, target, ...rulesets)` 设计成可变长 ruleset 参数（cascade 在 caller 那 flatten）。s09 的 `MergePermissions` 也产出一个 *flat* slice。s10 把这两层接起来：`Orchestrator.Permissions` 就是 *已经 cascade 过* 的 flat ruleset，每次 tool dispatch 直接 `Evaluate(name, target, o.Permissions)`，evaluator 不知道有 cascade 这回事。这就是 s09 README 那句"cascade is structural, evaluator is semantic"的兑现。

**Tool 接口完全不动**。s03 设计 `Tool.Execute(ctx, input) (string, error)` 时就预留了 ctx 和 err；s10 是第一个真正用 ctx（cancel propagation）+ err（in-band tool_result IsError）的消费者。如果 s03 当初让 Execute 只返回 string 没有 err，s10 这里就要被迫加 panic-recovery 之类的 ugly thing —— 这条印证了"早一节做对接口设计，晚一节回报"。

**接下来 s11-s14 的 angle**：
- s11 (skills) 给 system prompt 注入 SKILL.md 内容 —— 不改 Orchestrator，改 Request.System 怎么算。
- s12 (mcp) 让 Registry 装远程 tool —— 不改 Orchestrator，改 Tool 实现。
- s13 (lsp) 同 s12。
- s14 (cost & recovery) 给 Run 套一层 retry wrapper、把 Usage 攒起来记账 —— 不改 Orchestrator 内部，加 outer wrapper。

s10 的 Orchestrator 是后续所有改进的 *载体*。

## Try It

```bash
cd agents/s10-tool-loop

# 演示（确定性，无网络，2 轮 stream）：
go run .

# 5 个测试：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

5 个测试覆盖的场景：

1. **ZeroToolConversationCompletes** —— assistant 只回 text，没有 tool_use，loop 一轮就退。`provider.callCount==1`、trail 长 2 (initial + assistant)。基线测试。
2. **OneToolRoundTrip** —— assistant tool_use("echo") → tool_result → assistant end_turn。trail 长 4，Stream 调用 2 次。验证 Provider.Stream → Tool.Execute → 下一次 Provider.Stream 看到结果这条端到端线。
3. **TwoConsecutiveToolCalls** —— iter 1: echo("first") → iter 2: echo("second") → iter 3: end_turn。trail 长 6 (initial + 3 asst + 2 user-result)，Stream 调用 3 次。证明 inter-iteration trail 增长正确。
4. **PermissionDenyProducesErrorResult** —— ruleset `echo:* deny`。tool **不执行**，但 Run 返回 nil err，trail 里 tool_result Part 带 `IsError=true` + "permission denied"。证明 deny 是 in-band 信号。
5. **MaxIterationsExceeded** —— assistant 永远 ask echo，`MaxIterations: 1`。Run 返回 `ErrMaxIterationsExceeded`，Stream 只调用 1 次（cap 在 iter 1 开始前生效），trail 末尾是合成的 user-results Message —— caller 用更高的 cap 重 Run 可以续上。

## Upstream Source Reading

s10 mirror 的是 `packages/opencode/src/session/processor.ts`。整个文件 837 行，是 opencode 中 *最* 复杂的单文件之一 —— 它把 LLM 流式调用、工具派发、权限询问、snapshot、retry policy、错误恢复、event 系统全部组织起来。s10 取的是 *核心骨架*（Result + Handle + ProcessorContext + process），把 snapshot / retry / event 系统 / overflow 全部留给后续 session。

```ts
// upstream:packages/opencode/src/session/processor.ts L34-L82 + L734-L802

// L34 — 三种返回结果。s10 简化成 (trail, err) 两值；compact / continue
// 不出现在我们的接口里（continue 是 caller 的隐式行为，compact 留给 s14）。
export type Result = "compact" | "stop" | "continue"

// L38-L54 — Handle 接口。process(streamInput) 跑一轮迭代返 Result；
// caller (session.ts) 根据 Result 决定要不要再 process 一遍。
// 我们 Go 这边 Orchestrator.Run 直接做完 *所有* 迭代，不暴露 Handle。
export interface Handle {
  readonly message: MessageV2.Assistant
  readonly updateToolCall: (
    toolCallID: string,
    update: (part: MessageV2.ToolPart) => MessageV2.ToolPart,
  ) => Effect.Effect<MessageV2.ToolPart | undefined>
  readonly completeToolCall: (
    toolCallID: string,
    output: { title: string; metadata: Record<string, any>; output: string; attachments?: MessageV2.FilePart[] },
  ) => Effect.Effect<void>
  readonly process: (streamInput: LLM.StreamInput) => Effect.Effect<Result>
}

// L73-L82 — 每次 Run 的可变状态。重点：toolcalls 是个 dict —— 因为
// upstream 的 tool 是 *并发* 跑的（"tool-call" event 来一个就 fork
// 一个），需要按 callID 找到 pending 的 Part。我们 Go 这边串行跑，
// 不需要这个 dict。
interface ProcessorContext extends Input {
  toolcalls: Record<string, ToolCall>
  shouldBreak: boolean
  snapshot: string | undefined
  blocked: boolean
  needsCompaction: boolean
  currentText: MessageV2.TextPart | undefined
  reasoningMap: Record<string, MessageV2.ReasoningPart>
}

// L734-L802 — 真正的 process 函数。三段：
//   (a) 建 stream + Stream.tap(handleEvent) + Stream.takeUntil(needsCompaction)
//   (b) 用 Effect.retry(SessionRetry.policy) 包一层（s14 的活）
//   (c) 末尾的 Result 三选一
const process = Effect.fn("SessionProcessor.process")(function* (streamInput: LLM.StreamInput) {
  ctx.needsCompaction = false
  ctx.shouldBreak = (yield* config.get()).experimental?.continue_loop_on_deny !== true

  return yield* Effect.gen(function* () {
    yield* Effect.gen(function* () {
      ctx.currentText = undefined
      ctx.reasoningMap = {}
      const stream = llm.stream(streamInput)

      yield* stream.pipe(
        Stream.tap((event) => handleEvent(event)),     // ← 每个 event 派发
        Stream.takeUntil(() => ctx.needsCompaction),    // ← overflow 早退
        Stream.runDrain,
      )
    }).pipe(
      Effect.onInterrupt(() => Effect.gen(function* () {
        aborted = true
        if (!ctx.assistantMessage.error) yield* halt(new DOMException("Aborted", "AbortError"))
      })),
      Effect.retry(SessionRetry.policy({ /* ... s14 ... */ })),
      Effect.catch(halt),
      Effect.ensuring(cleanup()),
    )

    // ★ Result 三选一 —— 这是 s10 Orchestrator.Run 末尾对应的逻辑
    if (ctx.needsCompaction) return "compact"               // s14 才加
    if (ctx.blocked || ctx.assistantMessage.error) return "stop"  // 我们的 nil-err return
    return "continue"                                        // 我们的 for-loop 继续
  })
})

// L336-L395 — "tool-call" event handler。这是 *并发* 派发 tool 的入口
// （实际 execute 在 AI SDK 里、由 stream pipeline 异步触发；processor
// 这里只负责更新 Part state 和 doom-loop 检测）。
//
// L370-L394 是 doom-loop 检测：看最后 3 个 part，如果都是同一个 tool 同
// 一个 input → 弹 permission.ask 问用户。我们没做这个；s14 的 retry
// 分类会用类似机制。
case "tool-call": {
  if (ctx.assistantMessage.summary) {
    throw new Error(`Tool call not allowed while generating summary: ${value.toolName}`)
  }
  yield* updateToolCall(value.toolCallId, (match) => ({
    ...match,
    tool: value.toolName,
    state: { ...match.state, status: "running", input: value.input, time: { start: Date.now() } },
  }))

  // doom-loop: 同 tool 同 input 连续 3 次 → 问用户
  const parts = MessageV2.parts(ctx.assistantMessage.id)
  const recentParts = parts.slice(-DOOM_LOOP_THRESHOLD)
  if (
    recentParts.length === DOOM_LOOP_THRESHOLD &&
    recentParts.every(
      (part) => part.type === "tool" && part.tool === value.toolName &&
                part.state.status !== "pending" &&
                JSON.stringify(part.state.input) === JSON.stringify(value.input),
    )
  ) {
    const agent = yield* agents.get(ctx.assistantMessage.agent)
    yield* permission.ask({
      permission: "doom_loop",
      patterns: [value.toolName],
      sessionID: ctx.assistantMessage.sessionID,
      metadata: { tool: value.toolName, input: value.input },
      always: [value.toolName],
      ruleset: agent.permission,
    })
  }
  return
}
```

逐行注释（重点行）：

- **L34 Result 枚举** —— 三种结果。我们 Go 这边砍掉 "continue"（变成 for 循环的隐式继续）和 "compact"（s14 的活），只剩 "stop"（nil err 返回）和 "撞顶"（ErrMaxIterationsExceeded）。
- **L38-L54 Handle 接口** —— upstream 把 process 设计成 *单步* 操作，caller 反复调用直到 Result != "continue"。这种设计利于 caller 在两步之间插逻辑（cancel、UI 更新、persistence）。我们 Go 这边一把梭：Orchestrator.Run 内部把所有循环跑完，return 完整 trail。Trade-off：少了 caller 的中段控制权（要 cancel 只能用 ctx），换来更简单的 API 表面。
- **L73-L82 ProcessorContext** —— 这是 *每次 process 调用共享的可变状态*。注意 `toolcalls` 是个 dict —— 这是因为 upstream 的 tool 是 *并发* 跑的，"tool-call" event 收到一个就 fork 一个 Promise 去跑，结果回来时需要 callID 索引。我们串行跑就不需要这个 dict —— `runOneTool` 是同步函数。
- **L734-L745 stream.pipe** —— Stream.tap(handleEvent) 是关键：每个 event 都过一遍 handleEvent，handleEvent 内部一个大 switch 处理 "text-delta" / "tool-call" / "tool-result" 等。我们 Go 这边把 "switch on event type" 留在 `consumeOne` 里（drain 阶段），把 "tool dispatch" 抽到 `runOneTool`（drain 之后）—— 因为我们不并发跑 tool，可以两步分开做。
- **L750-L760 Effect.onInterrupt + Effect.retry + Effect.catch** —— 三层错误处理：abort、retry policy（s14）、最终 halt。我们 Go 这边只做最简单的 ctx.Err() 检查 —— retry / overflow 留给 s14。
- **L798-L800 Result 三选一** —— 这是 s10 Orchestrator.Run 最像的部分：
  - `if (ctx.needsCompaction) return "compact"` → 我们没有
  - `if (ctx.blocked || ctx.assistantMessage.error) return "stop"` → 我们的"自然 end_turn 时 return trail, nil"
  - `return "continue"` → 我们的"`for iter` 继续"
- **L370-L394 doom-loop 检测** —— upstream 的"同 tool 同 input 连续 3 次"保护。我们没做（s14 的 retry 分类会用类似机制）。注意 L386-L394 调用 `permission.ask` 是 *阻塞* 的（在 UI 那边等用户回复）—— 我们 headless 没有 UI，干脆把整个分支去掉。

permalink：

- Result + Handle (L34-L54): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L34-L54>
- ProcessorContext (L73-L82): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L73-L82>
- tool-call handler + doom-loop (L336-L395): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L336-L395>
- process function (L734-L802): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/processor.ts#L734-L802>

我们留下了什么、砍了什么：

- **留下** —— 5 步迭代骨架（stream → drain → tool dispatch → result feedback → loop）、Permission gate 在 tool 执行前调用、tool error 是 in-band 反馈、MaxIterations 安全上限、Stream.Close 用 defer 保证。
- **暂时砍掉** —— Effect runtime / Layer / Service 全套（Go 用 plain struct + ctx）、Snapshot 持久化（s07 territory）、Plugin.trigger 钩子（s11+ territory）、SessionRetry policy（s14）、isOverflow / needsCompaction 触发 compaction（s14）、DOOM_LOOP_THRESHOLD 检测（s14 也会做类似分类）、SessionEvent v2 双写（整个 v2 schema 都是另外的事）、并发 tool 派发（我们串行跑）。
- **向前兼容** —— 加 retry wrapper 不需要改 Orchestrator 内部（外面套就行）；加 cost 追踪只需要消费 trail 里每个 assistant Message 的 Usage 字段；加并发 tool 只需要把 `runOneTool` 调用循环改成 errgroup。Orchestrator 的 5 步骨架会一直在。

opencode session-processor 层的阅读顺序：

1. `packages/opencode/src/session/processor.ts` L34-L82 —— Result / Handle / ProcessorContext shape（s10 Orchestrator 的母本，本节正文）
2. `packages/opencode/src/session/processor.ts` L734-L802 —— process 函数（s10 Orchestrator.Run 的核心 mirror）
3. `packages/opencode/src/session/processor.ts` L229-L640 —— handleEvent 的大 switch（s10 把里面的 "text/reasoning/tool-input" 抽进 consumeOne，把 "tool-call/result/error" 抽进 runOneTool）
4. `packages/opencode/src/session/llm.ts` L100-L200 —— LLM.stream 的实现（s06 mirror 的母本；s10 复用了 s06 的 streaming assembly 算法）
5. `packages/opencode/src/permission/index.ts` —— `permission.ask` 的实现（s10 的 Ask 默认 → Allow 是简化；真实路径要等用户回复）
