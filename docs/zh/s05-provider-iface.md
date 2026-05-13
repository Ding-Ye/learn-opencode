---
title: "s05 · Provider 抽象"
chapter: 5
slug: s05-provider-iface
est_read_min: 11
---

# s05 · Provider 抽象

> 本章教什么：把 s01 的 *一次阻塞调用* 升级成 *interface + 流*。这是后面每节都要靠的 LLM 调用形态 —— 一个 `Provider` 接口、一个 `Stream` 迭代器，外加 Anthropic 的 SSE 解析做出第一个具体实现。Phase G 加 OpenAI 时不动 caller，只加一个新结构体。

---

## Problem

s01 的 `CreateMessage(ctx, req) (*Resp, error)` 撑得起两件事：印证 Anthropic 的 wire format，跑一遍 happy path。再多就不行了：

- **流式才是真实形态。** Anthropic 的 `tool_use` 块跨多个 SSE 帧到达；input JSON 是 token-by-token 流回来的。要等整个 response 都到齐再看，意味着 latency 直接累积成「response 长度」。
- **决策必须中途做。** 一旦 LLM 决定调一个工具，loop 得先过权限闸（s04），决定是 allow / deny / ask；deny 的话整段后续都不用收。一次性返回的 API 没地方插这个钩子。
- **vendor lock 一旦写进调用点就拔不出来了。** s10 的 loop 如果直接 `import "anthropic"`，那 Phase G 加 OpenAI / Bedrock 就要重写 loop —— 而 loop 跟 vendor 完全无关，是 Provider 抽象漏出来了。

s01 的接口形态恰好把这三件事全堵死了。s05 把接口形态本身换掉。

## Solution

`Provider` 是一个接口，一个方法：

```go
type Provider interface {
    Stream(ctx context.Context, req Request) (Stream, error)
}
```

`Stream` 是一个 pull-based 的 iterator：

```go
type Stream interface {
    Next() (Event, error)   // 流结束时返回 io.EOF
    Close() error
}
```

`Event` 是个 tagged union：`EventText`（文本 delta）、`EventToolUse`（buffered 完整的工具调用）、`EventReasoning`（extended-thinking 块）、`EventFinish`（终止事件 + final Usage）。

Anthropic 的具体实现 `AnthropicProvider` 干两件事：

1. POST 到 `/v1/messages`，body 里加 `"stream": true`，header 加 `Accept: text/event-stream`。
2. 一个 `*anthropicStream` 把 SSE 字节读出来 —— 一行 `event: ...` + 一行 `data: {...JSON...}` + 空行 —— 把每种上游事件类型（`message_start` / `content_block_start` / `content_block_delta` / `content_block_stop` / `message_delta` / `message_stop`）翻译成我们的 Event union。

整个模块 ~400 LOC，4 个用 `httptest` stub server 的测试。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s05 Provider 抽象                                                      │
│                                                                        │
│   var p Provider = NewAnthropicProvider(apiKey, model)                 │
│   stream, err := p.Stream(ctx, Request{...})    ──── POST /v1/messages │
│                                                       (stream=true)    │
│                                                                        │
│   Anthropic SSE 字节回来 ──→  *anthropicStream                          │
│                                                                        │
│   for {                              ┌─ message_start    → 攒 input_tokens
│       ev, err := stream.Next()       ├─ content_block_start → 记录 block 类型
│       if errors.Is(err, io.EOF) {    ├─ content_block_delta:
│           break                      │     text_delta     → EventText
│       }                              │     input_json_delta → 攒到 buffer
│       switch ev.Type {               │     thinking_delta  → EventReasoning
│       case EventText:    ...         ├─ content_block_stop → tool_use 块发 EventToolUse
│       case EventToolUse: ...         ├─ message_delta     → 攒 output_tokens
│       case EventFinish:  ...         ├─ message_stop      → EventFinish (Usage)
│       }                              └─ (下一次 Next)      → io.EOF
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**三个 load-bearing 设计**：

1. **`Next()` 在 clean end 返回 `io.EOF`。** 这是 Go idiomatic 的「流结束」信号 —— `(*os.File).Read`、`(*bufio.Scanner).Scan`、channel close 都是这个形状。s06 / s10 / s14 的 loop 全部会写 `for { ev, err := stream.Next(); if errors.Is(err, io.EOF) { break } }`，所以这个契约是固定的。
2. **tool_use 的 input 在 `content_block_stop` 之前都是 buffered 的。** Anthropic 把 input JSON 切成 N 个 `input_json_delta` 推过来，我们攒在 `contentBlockBuffer.jsonAcc` 里，到 `content_block_stop` 才一次性发一个 `EventToolUse`。理由：consumer 没办法用半截 input 调工具，每个 consumer 各自实现 buffering 是重复劳动。
3. **`EventFinish` 和 `io.EOF` 是分开两次 `Next()` 调用。** Finish 事件携带数据（Usage），EOF 是循环退出信号 —— 把它们合并会逼每个 consumer 加一个「拿到 usage 没」的状态位。

**Usage 跨两个 SSE 事件的拼接**：`message_start` 携带 `input_tokens`，`message_delta` 携带 `output_tokens`。我们在 `*anthropicStream.usage` 里累加，到 `message_stop` 时一次性 emit。Consumer 看到的是单一一个 `Usage`，跟 vendor 无关。

## What Changed (vs. s01)

s01 一次阻塞调用，s05 同样的 wire shape 但拉成流：

```diff
 // s01: 阻塞，一来一回。
-resp, err := p.CreateMessage(ctx, req)
-for _, b := range resp.Content {
-    if b.Type == "text" { fmt.Println(b.Text) }
-}

+// s05: pull-based 流，事件来一个处理一个。
+stream, err := p.Stream(ctx, req)
+defer stream.Close()
+for {
+    ev, err := stream.Next()
+    if errors.Is(err, io.EOF) { break }
+    if err != nil { return err }
+    switch ev.Type {
+    case EventText:    fmt.Print(ev.Text)
+    case EventToolUse: handleTool(ev.ToolUse)        // s10 在这里 dispatch
+    case EventFinish:  recordUsage(ev.Usage)         // s14 在这里计费
+    }
+}
```

HTTP request 几乎一样：相同的 endpoint、相同的 headers（`x-api-key`、`anthropic-version: 2023-06-01`、`Content-Type: application/json`）、相同的 JSON body —— 多一个 `"stream": true` 字段、多一个 `Accept: text/event-stream` header。Response Content-Type 从 `application/json` 变成 `text/event-stream`。

抽象边界变化：s01 的 `Provider` 接口写法是 `CreateMessage(ctx, req) (*Resp, error)`；s05 改成 `Stream(ctx, req) (Stream, error)`。后者覆盖前者 —— 一个 trivial 实现可以把 stream drain 完拼成一个 response，所以 s01 的能力是 s05 的 strict subset。每节往后走，能力是单调增加的。

`main.go` 里特意写 `var p Provider = NewAnthropicProvider(...)` 而不是 `p := NewAnthropicProvider(...)` —— 强调下面所有代码只看到 interface，看不到 vendor。Phase G 加 `OpenAIProvider` 时，只改这一行。

## Try It

```bash
cd agents/s05-provider-iface

# 真实流式 demo（要 ANTHROPIC_API_KEY）：
export ANTHROPIC_API_KEY=sk-ant-...
go run . hello in three words

# 4 个测试，全用 httptest stub server 喂 canned SSE 字节，无网络。
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

测试覆盖的 4 个场景：

1. **text-only stream** —— `EventText` 序列正确串联。
2. **tool_use stream** —— 多个 `input_json_delta` 攒成一个 `EventToolUse`，name/id/input 三件套都解出来。
3. **reasoning stream** —— `thinking_delta` 解为 `EventReasoning`。
4. **message_stop → EventFinish + io.EOF** —— 终止契约：finish 事件先到，下一次 `Next()` 必须返回 `io.EOF`，Usage 把跨事件的 input/output token 合并好。

## Upstream Source Reading

s05 mirror 的是 opencode 的 `packages/opencode/src/provider/provider.ts`。整个文件 1792 行 —— 主要的认知负担在 23 行的 `BUNDLED_PROVIDERS` 字典上：每个键是一个 npm 包名，值是个 thunk，里面用 `import()` 动态加载该 vendor 的 SDK。这是 opencode 整套 multi-provider 设计的入口，所有 vendor 抽象都从这里展开。

```ts
// upstream:packages/opencode/src/provider/provider.ts L87-L117
type BundledSDK = {
  languageModel(modelId: string): LanguageModelV3
}

const BUNDLED_PROVIDERS: Record<string, () => Promise<(opts: any) => BundledSDK>> = {
  "@ai-sdk/amazon-bedrock": () => import("@ai-sdk/amazon-bedrock").then((m) => m.createAmazonBedrock),
  "@ai-sdk/anthropic": () => import("@ai-sdk/anthropic").then((m) => m.createAnthropic),
  "@ai-sdk/azure": () => import("@ai-sdk/azure").then((m) => m.createAzure),
  "@ai-sdk/google": () => import("@ai-sdk/google").then((m) => m.createGoogleGenerativeAI),
  "@ai-sdk/google-vertex": () => import("@ai-sdk/google-vertex").then((m) => m.createVertex),
  "@ai-sdk/google-vertex/anthropic": () =>
    import("@ai-sdk/google-vertex/anthropic").then((m) => m.createVertexAnthropic),
  "@ai-sdk/openai": () => import("@ai-sdk/openai").then((m) => m.createOpenAI),
  "@ai-sdk/openai-compatible": () => import("@ai-sdk/openai-compatible").then((m) => m.createOpenAICompatible),
  "@openrouter/ai-sdk-provider": () => import("@openrouter/ai-sdk-provider").then((m) => m.createOpenRouter),
  "@ai-sdk/xai": () => import("@ai-sdk/xai").then((m) => m.createXai),
  "@ai-sdk/mistral": () => import("@ai-sdk/mistral").then((m) => m.createMistral),
  "@ai-sdk/groq": () => import("@ai-sdk/groq").then((m) => m.createGroq),
  "@ai-sdk/deepinfra": () => import("@ai-sdk/deepinfra").then((m) => m.createDeepInfra),
  "@ai-sdk/cerebras": () => import("@ai-sdk/cerebras").then((m) => m.createCerebras),
  "@ai-sdk/cohere": () => import("@ai-sdk/cohere").then((m) => m.createCohere),
  "@ai-sdk/gateway": () => import("@ai-sdk/gateway").then((m) => m.createGateway),
  "@ai-sdk/togetherai": () => import("@ai-sdk/togetherai").then((m) => m.createTogetherAI),
  "@ai-sdk/perplexity": () => import("@ai-sdk/perplexity").then((m) => m.createPerplexity),
  "@ai-sdk/vercel": () => import("@ai-sdk/vercel").then((m) => m.createVercel),
  "@ai-sdk/alibaba": () => import("@ai-sdk/alibaba").then((m) => m.createAlibaba),
  "gitlab-ai-provider": () => import("gitlab-ai-provider").then((m) => m.createGitLab),
  "@ai-sdk/github-copilot": () =>
    import("@opencode-ai/core/github-copilot/copilot-provider").then((m) => m.createOpenaiCompatible),
  "venice-ai-sdk-provider": () => import("venice-ai-sdk-provider").then((m) => m.createVenice),
}
```

逐行注释（重点行）：

- **L87-L89 `BundledSDK`** —— 这是 opencode 看到每个 vendor SDK 的 *形状*：一个 `languageModel(id)` 方法返回 `LanguageModelV3`（Vercel AI SDK 的内部接口）。我们的 Go 翻译就是 `Provider` 接口本身 —— 一个方法 `Stream(ctx, Request)`。
- **L91 `Record<string, () => Promise<factory>>`** —— 三层 indirection：字符串键 → 一个 thunk → thunk 里 `import()` → import 解决出 factory func → factory func 接受 options 返回 BundledSDK。Go 没有 dynamic import，所以我们扁平化成 *构造函数*：`NewAnthropicProvider(apiKey, model)` 直接返回一个满足 Provider 的东西。Phase G 会有 `NewOpenAIProvider`、`NewBedrockProvider`，名字不同、签名相同。
- **L92-L93 Anthropic / Bedrock 入口** —— 这两行是 s05 直接对应的 vendor。我们的 `AnthropicProvider.Stream` 干的事就是 `createAnthropic(opts).languageModel(modelID)` 之后 AI SDK 内部干的事 —— 我们手写 SSE 解析，不依赖 SDK。
- **L99-L100 OpenAI 入口** —— Phase G 的 sibling。OpenAI 的 wire 是 `/v1/chat/completions` + 不一样的 SSE 事件命名（`data: [DONE]` 而不是 `message_stop`），但我们的 Provider 接口完全不变 —— 同一个 `Request` 进、同一个 `Stream` 出。这就是 interface 的全部意义。
- **L101-L116** —— 22 个其它 vendor。注意 OpenRouter 和 Gateway 都是 *meta* providers，把请求路由到底层 vendor。Go 翻译里它们要么也是独立结构体（如果有自己的 wire 差异），要么是 `OpenAIProvider` 加 base URL override（OpenRouter 走 OpenAI-compatible API）。

permalink：

- BUNDLED_PROVIDERS（L87-L117）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/provider/provider.ts#L87-L117>
- custom loaders（L119-L150）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/provider/provider.ts#L119-L150>

我们留下了什么、砍了什么：

- **留下** —— Provider-as-interface 的形态、BundledSDK 的「一个方法」契约（我们的 `Stream`）、Anthropic 的 wire shape 字节级一致（请求 body、headers、SSE 事件类型 → Event union 翻译）。
- **暂时砍掉** —— 22 个非 Anthropic 的 vendor（Phase G 加 OpenAI 是第二个；其余按需）；`wrapSSE()` 的 timeout cancellation（我们用 `context.Context` 的 deadline 替代）；custom loaders（`L149+` 的 per-provider 重写逻辑，比如 Azure / Vertex 的特殊 model 选择）；plugin-installed providers；`ProviderTransform` 的请求改写。
- **向前兼容** —— Phase G 加 OpenAI 时，加一个 `provider_openai.go` 文件、一个 `OpenAIProvider` 结构体、一个 `NewOpenAIProvider` 构造函数。s06 / s10 / s14 一行不改。这就是这个抽象 *值得做* 的证明 —— 加一个 vendor 的 cost 是 O(1)，不是 O(consumer)。

opencode provider 层的阅读顺序：

1. `packages/opencode/src/provider/provider.ts` L87-L117 —— `BUNDLED_PROVIDERS` 字典（本节 s05）
2. `packages/opencode/src/provider/provider.ts` L39-L85 —— `wrapSSE()` 的 timeout cancellation（s05 用 ctx 替代）
3. `packages/opencode/src/session/llm.ts` L100-L200 —— `streamText()` 怎么从 loop 调（s06）
4. `packages/opencode/src/provider/provider.ts` L149+ —— custom loaders（Phase G）
5. `packages/opencode/src/session/processor.ts` L34-L150 —— Event 怎么变成 Part、tool 怎么 dispatch（s10）
