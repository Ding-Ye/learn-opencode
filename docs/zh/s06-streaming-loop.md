---
title: "s06 · 流式循环"
chapter: 6
slug: s06-streaming-loop
est_read_min: 12
---

# s06 · 流式循环

> 本章教什么：把 s05 的 `Provider.Stream` *消费者* 写出来。一个 `Loop` 把流式 Event 一个一个聚合成一个 `Message` 的 Parts —— N 个相邻 text delta 折成一个 `TextPart`、buffered 完整的 tool_use 直接成一个 `ToolUsePart`、reasoning chunk 折成一个 `ReasoningPart`、`EventFinish` 把 Usage 记到 Message 上。这是流式 agent 取代「等整个 response 再处理」模式的 *最小 bridge*。

---

## Problem

s05 把 LLM 调用从「一次阻塞返回」升级成了「pull-based 的事件流」。但流出来之后呢？拿这个流的代码，要把它变成什么形态才能给后面的代码用？

- **消费者不能直接看 Event。** Event 是 *wire 层* 的抽象 —— 「下一帧 SSE 给了我什么」。但 s07 要持久化的是 `Message` of `Parts`；s10 的工具循环要看的是「这条 assistant 消息到底有几个 tool_use Part」；s14 的 cost 追踪要看的是 `Message.Usage`。每个消费者都需要 *聚合后的* 形态。
- **聚合规则不平凡。** 一个 prose 段是 N 个相邻 text delta —— 这些必须折成一个 `TextPart`，否则 s07 的 SQLite 一次 assistant 回复要存 N 行（而不是 1 行），且没有 row 边界对应「一段完整的话」。但是中间夹了一个 tool_use，后面的 text 又要 *新开* 一个 `TextPart`，因为这是不同的语义边界。
- **如果让每个消费者各自实现这个聚合**，代码重复，且每个地方都可能写错（漏处理 reasoning、漏处理 finish、漏处理 abort）。
- **abort 不能等流跑完。** 用户按 Ctrl-C，必须立刻停 —— 不能 silently 把剩下的 Event 收完丢掉。这要求消费者每次 `Next()` 之间检查 `ctx.Err()`。

s06 的工作就是写 *那一个* 消费者：`Loop`。它的职责到此为止 —— 不 dispatch 工具（s10）、不查权限（s04 / s10）、不持久化（s07）、不重试（s14）。每节加一个机制，不重写流式层。

## Solution

`Loop` 是一个结构体加一个方法：

```go
type Loop struct { Provider Provider }

func (l *Loop) Consume(ctx context.Context, req Request) (*Message, error)
```

`Consume` 干的事：

1. `stream, err := l.Provider.Stream(ctx, req)`，错就直接返回。
2. `defer stream.Close()`。
3. 在 `for` 循环里 `stream.Next()` 拉 Event，直到 `io.EOF` 返回 `(*Message, nil)`。
4. 每次 `Next()` 前先 `ctx.Err()` 检查 —— cancel 立刻返 `context.Canceled`，不返半截 Message。
5. 按 Event.Type switch，按聚合规则 append/extend `msg.Parts`：
   - `EventText` —— 如果上一个 Part 是 `PartText`，extend 它；否则 append 一个新 `TextPart`。
   - `EventToolUse` —— 永远 append 一个新 `ToolUsePart`（input 已经在 s05 的 Provider 层 buffered 完）。如果 `Name` 为空，返 error（明确指出 Provider 实现的 bug）。
   - `EventReasoning` —— 跟 text 同样的 extend / append 规则，对应 `PartReasoning`。
   - `EventFinish` —— 把 `Usage` 拷到 `msg.Usage`。下一次 `Next()` 应该返 `io.EOF`。
6. 推断 `StopReason`：最后一个 Part 是 `PartToolUse` 就是 `"tool_use"`（s10 的 loop 知道要再问），否则 `"end_turn"`。

整个模块 ~500 LOC：provider/parts/loop ~370 行实现 + ~130 行 fake provider + 测试。4 个测试，全部用 `fakeProvider`，无网络。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s06 Loop.Consume                                                       │
│                                                                        │
│   loop := &Loop{Provider: ...}                                          │
│   msg, err := loop.Consume(ctx, Request{...})                           │
│                                                                        │
│   stream, _ := provider.Stream(ctx, req)                                │
│   defer stream.Close()                                                  │
│   msg := &Message{Role: RoleAssistant}                                  │
│   trailing := PartUnknown                                               │
│                                                                        │
│   for {                                                                │
│     if ctx.Err() != nil { return nil, ctx.Err() }   ← abort hatch     │
│     ev, err := stream.Next()                                            │
│     if errors.Is(err, io.EOF) { return msg, nil }   ← clean end       │
│                                                                        │
│     switch ev.Type {                                                    │
│     case EventText:                                                     │
│       if trailing == PartText { extend last TextPart }                  │
│       else                    { append new  TextPart  }                 │
│     case EventToolUse:        { append new  ToolUsePart (full input) } │
│       if ev.ToolUse.Name == "" → return error "tool name"               │
│     case EventReasoning:                                                │
│       if trailing == PartReasoning { extend } else { append new }       │
│     case EventFinish:         { msg.Usage = ev.Usage; infer StopReason }│
│     }                                                                  │
│     trailing = <kind we just appended>                                  │
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**四个 load-bearing 设计**：

1. **adjacent-same-kind 折叠，跨 kind 断开。** N 个 text delta 折成 1 个 `TextPart`；中间夹了 tool_use，后面的 text 必须 *新开* 一个 `TextPart`。这是 s10 「这条消息以什么结尾 → 决定是 dispatch 工具还是结束 loop」判断的根基 —— 如果文本被错误连接，语义边界就丢了。
2. **`ctx.Err()` 在每次 `Next()` 之前检查。** 用户 Ctrl-C → ctx canceled → Loop 立刻返 `context.Canceled`，*不返* 半截 Message。半截 Message 很危险 —— 调用者会忍不住「先用上看看」，然后下一节 s07 就会把半截消息持久化到 SQLite。
3. **`EventToolUse` 缺 `Name` 直接 fail。** Provider 实现保证 buffered 完才发 `EventToolUse`；如果发了一个空 Name，就是 bug。在 Loop 层 fail loud，错误指向 Provider；放过去到 s10 dispatcher 才 fail，错误是 `unknown tool ""`，不知道是哪个环节的事。
4. **`EventFinish` → `io.EOF` 是两次 `Next()`。** 直接继承 s05 的契约 —— Finish 携带 Usage，EOF 是循环退出。Loop 在拿到 Finish 后不 break，继续走下一轮 `Next()` 拿 EOF。

**为什么 Loop 是 ~150 行**：因为它只做聚合。每个其他职责都被推到下一节 —— s10 加 dispatch、s07 加持久化、s14 加 retry。这是渐进教学的 *物理* 体现：每节真的只动一个文件。

## What Changed (vs. s05)

s05 的 `main.go` 演示是直接在 `for` 循环里 print 每个 Event：

```diff
 // s05: 把 Event 直接打到屏幕上 —— 演示 Stream 能拉。
-stream, _ := p.Stream(ctx, req)
-defer stream.Close()
-for {
-    ev, err := stream.Next()
-    if errors.Is(err, io.EOF) { break }
-    switch ev.Type {
-    case EventText:    fmt.Print(ev.Text)
-    case EventToolUse: fmt.Printf("[tool_use] %s\n", ev.ToolUse.Name)
-    case EventFinish:  fmt.Printf("[tokens: %d/%d]\n", ev.Usage.InputTokens, ev.Usage.OutputTokens)
-    }
-}

+// s06: 把 Event 聚合成一个 Message of Parts —— 把流变成可持久化、可 dispatch、可 inspect 的形态。
+loop := &Loop{Provider: p}
+msg, err := loop.Consume(ctx, req)
+if err != nil { return err }
+// msg.Parts 是 s07 持久化的对象、s10 dispatch 工具的对象、s14 计费 Usage 的对象。
+for _, part := range msg.Parts {
+    switch part.Kind {
+    case PartText:    fmt.Println(part.Text.Text)
+    case PartToolUse: dispatch(part.ToolUse)        // s10 在这里
+    }
+}
```

Provider 接口本身一行没改 —— 这就是 s05 「Stream 是抽象」做对了的证明。s06 加的是 *消费者*，没动生产者。

抽象边界：s05 之前没有 *消费者层* 这一概念 —— 它的 main 循环就是一段临时的 print 代码。s06 把那段代码升级成 `Loop`，从 demo-grade 升级成 production-grade（有错误处理、有 cancel、有验证、有结构化输出）。s10 的工具循环会 *复用* 这个 `Consume`，在外面再裹一个「dispatch 工具 + 把结果作为 user message append + 再调一次 Provider」的更大循环。

需要注意的小细节：`provider.go` 里的 `ToolUseEvent`（wire 层）和 `parts.go` 里的 `ToolUsePart`（聚合后的 Part 变体）是两个 struct。它们字段几乎一样，但语义不同 —— 一个是「wire 这次给了我什么」，一个是「我装进 Message 里的是什么」。s10 会做这两层之间的反向翻译（把 `ToolResultPart` 翻译成 wire 的 `tool_result` ContentBlock）。

## Try It

```bash
cd agents/s06-streaming-loop

# 演示（确定性，无网络）：
go run .

# 4 个测试，全用 fakeProvider 喂 scripted Events，无网络：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

测试覆盖的 4 个场景：

1. **text-only stream** —— 3 个 EventText delta 折成 1 个 `TextPart`，Usage 正确，StopReason 推断为 `"end_turn"`。
2. **interleaved tool_use + text** —— text + tool_use + text 三个 Event 变成 *3 个 Parts*（不是 2 个文本拼接 + 1 个 tool_use）。这是 s10 的根基。
3. **AbortContext mid-stream** —— `ctx.Cancel()` 在第二个 Event 之前触发，`Consume` 返 `context.Canceled`，Message 是 `nil`（不是半截）。
4. **malformed Event** —— `EventToolUse` 缺 Name，Loop fail fast，error message 包含 "tool name"，不返半截 Message。

## Upstream Source Reading

s06 mirror 的是 opencode 的 `packages/opencode/src/session/llm.ts`。整个文件 469 行，s06 关心的是 L100-L200 这一段 —— 把 `Provider.Stream` 调出去之前的 *准备*：组合 system prompt、merge options、过 plugin hook。我们把这一段 *砍光*（因为 s06 教的是 *消费者*，不是 *准备方*），只保留聚合层。

```ts
// upstream:packages/opencode/src/session/llm.ts L100-L143

// TODO: move this to a proper hook
const isOpenaiOauth = item.id === "openai" && info?.type === "oauth"

const system: string[] = []
system.push(
  [
    // use agent prompt otherwise provider prompt
    ...(input.agent.prompt ? [input.agent.prompt] : SystemPrompt.provider(input.model)),
    // any custom prompt passed into this call
    ...input.system,
    // any custom prompt from last user message
    ...(input.user.system ? [input.user.system] : []),
  ]
    .filter((x) => x)
    .join("\n"),
)

const header = system[0]
yield* plugin.trigger(
  "experimental.chat.system.transform",
  { sessionID: input.sessionID, model: input.model },
  { system },
)
// rejoin to maintain 2-part structure for caching if header unchanged
if (system.length > 2 && system[0] === header) {
  const rest = system.slice(1)
  system.length = 0
  system.push(header, rest.join("\n"))
}

const variant =
  !input.small && input.model.variants && input.user.model.variant
    ? input.model.variants[input.user.model.variant]
    : {}
const base = input.small
  ? ProviderTransform.smallOptions(input.model)
  : ProviderTransform.options({
      model: input.model,
      sessionID: input.sessionID,
      providerOptions: item.options,
    })
const options = mergeOptions(mergeOptions(mergeOptions(base, input.model.options), input.agent.options), variant)
```

逐行注释（重点行）：

- **L102-L114 system prompt 组合** —— 4 个来源：agent.prompt（s09 引入）、provider 默认（per-vendor 默认 prompt）、本次调用 system override、用户最后一条消息的 system。`filter((x) => x).join("\n")` 把空字符串去掉再 join。s06 的 Request.System 是单字符串 —— 我们 *不组合*。s09 加 agent registry 的时候才会需要这个层级。
- **L116-L121 plugin hook `experimental.chat.system.transform`** —— 让用户写的 plugin 在 LLM 调用之前重写 system prompt 数组。s06 没有 plugin layer。
- **L122-L127 prompt caching 优化** —— Anthropic 的 prompt cache 只 honor 第一个 system block。opencode 把 header 锁住，把所有后续 prompt 折进 block #2，这样跨多轮缓存能命中。s14 的 cost 章节可能会 revisit。
- **L129-L132 variants** —— 一个 model id（`claude-sonnet-4-5`）可以有 high-effort / low-effort 两个 variant（不同 temperature 等）。s06 的 Request 直接拿 `Model string`，不分 variant。
- **L133-L139 ProviderTransform.options** —— 返回 per-provider 的 options blob（Anthropic 是 `{anthropic: {...}}`、OpenAI 是 `{openai: {...}}`）。s06 的 Request 没有 `Options` 字段 —— provider-specific 旋钮被 s05 的接口故意砍掉，保持 cross-vendor 表面最小。Phase G 加回来在 concrete Provider 上的 `AnthropicOptions` / `OpenAIOptions`。
- **L140 4 层 deep merge** —— `base → model defaults → agent overrides → variant`，每层覆盖前一层。s06 的 Request 是个扁平的 6 字段 struct。这里要建立的 *心智升级*：这 4 层 merge 是 `Provider.Stream` *抽象掉* 给 consumer 的东西。Loop 不知道也不需要知道传了什么旋钮 —— 它只 care Event 回来。这就是 abstraction 的全部价值。

permalink：

- streamText prep（L100-L200）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L100-L200>
- streamText 实际 invocation（L336-L415）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L336-L415>
- AsyncIterable → Stream 包装（L418-L432）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L418-L432>

我们留下了什么、砍了什么：

- **留下** —— consumer 这一层的形态：从 `Stream` 拉 Event、聚合成 Parts、记 Usage、推断 StopReason。聚合规则跟 opencode 的 processor.ts reducer 行为一致（adjacent text 折叠、tool_use 不折叠、reasoning 折叠）。
- **暂时砍掉** —— system prompt 组合（s09）、provider options merge（用 vendor 默认）、plugin hooks（无 plugin layer）、variants（用 Model 字段）、tool dispatch（s10）、permission check（s10）、snapshot/diff（s07/s10）、retry（s14）、persistence（s07）。
- **向前兼容** —— s10 的工具循环 *复用* `Loop.Consume`，在外面套「dispatch 工具 + append result 作为 user message + 再调一次 Provider」的循环。s06 一行不改。s07 加持久化的时候，把 `Consume` 返回的 Message 扔到 SQLite 里。s14 加 retry 的时候，把 `Consume` 包在 retry wrapper 里。Loop 的接口是 `(ctx, req) → (*Message, error)`，没有副作用，retry 安全。

opencode session layer 的阅读顺序：

1. `packages/opencode/src/session/llm.ts` L100-L143 —— streamText 的准备（本节 s06 砍掉的部分）
2. `packages/opencode/src/session/llm.ts` L336-L415 —— streamText() 的实际调用（s05 已经手写 SSE）
3. `packages/opencode/src/session/processor.ts` L34-L150 —— Event reducer，s10 复用 s06 的 Loop 在这里加 dispatch
4. `packages/opencode/src/session/llm.ts` L418-L432 —— AsyncIterable → Stream（我们的 Stream interface 等价）
5. `packages/opencode/src/session/processor.ts` (tool dispatch) —— s10 的「dispatch + 反馈」
