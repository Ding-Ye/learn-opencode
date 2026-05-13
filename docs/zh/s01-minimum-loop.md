---
title: "s01 · 最小 agent loop"
chapter: 1
slug: s01-minimum-loop
est_read_min: 8
---

# s01 · 最小 agent loop

> 本章教什么：能跟 LLM 通话的最小 call site。我们这一章不构建 agent —— 我们构建 **agent 调用的那个东西**。如果这个原语没钉死，后面每一节都会继承错误的 wire-format 假设。

---

## Problem

opencode 是个 159K star 的编码 agent，它会流式输出、调度工具、持久化 session、评估权限、在 25+ LLM 厂商之间路由。从 183K LOC 的核心包顶端往下读，是绝佳的淹死自己的方式。

我们需要一页纸的锚点：发条消息给 Anthropic，拿到回复，打印出来。不流式。不带工具。不存 SQLite。不重试。如果这层形状错了，后面 `s02 message-parts`、`s05 provider-iface`、`s06 streaming-loop` 都得回头返工。

## Solution

整个东西就是两个接口和一次 HTTP 调用：

1. **`Provider` 是 interface（不是 struct）**，所以 `s05 provider-iface` 加 OpenAI 时，`main.go` 的调用点不用动一行字。这映射到 opencode 的 `BUNDLED_PROVIDERS` map（`packages/opencode/src/provider/provider.ts#L91-L117`）。
2. **`Message` / `ContentBlock` 直接用 Anthropic 的 wire 格式**，不另发明内部类型。opencode 通过 `@ai-sdk` 翻译层付出了隐藏成本；我们什么都不付，因为 Go 没有等价生态，在 s01 里硬造一套是过早抽象。
3. **`AnthropicProvider` 带一个可覆盖的 `baseURL`**，只有 `withBaseURL`（test helper）会改它。这是让 `provider_test.go` 用 `httptest` 跑，永不打真 API 的那个小动作。

## How It Works

```
┌────────────────────────────────────────────────────────────┐
│  s01 minimum loop                                          │
│                                                            │
│   main.go                                                  │
│      │                                                     │
│      │  组装 CreateMessageRequest{System, Messages}        │
│      ▼                                                     │
│   Provider.CreateMessage(ctx, req) ────► Anthropic /v1/    │
│      │                                   messages          │
│      │  HTTP 200, JSON                                     │
│      ▼                                                     │
│   *CreateMessageResponse                                   │
│      ├─ .Content[0].Text  ──► fmt.Println                  │
│      └─ .Usage           ───► fmt.Fprintf(os.Stderr, …)    │
└────────────────────────────────────────────────────────────┘
```

干活的 50 行在 `provider.go` 里：

```go
type Provider interface {
    CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error)
}

func (a *AnthropicProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
    if req.Model == "" { req.Model = a.model }
    if req.MaxTokens == 0 { req.MaxTokens = 4096 }

    body, _ := json.Marshal(req)
    httpReq, _ := http.NewRequestWithContext(ctx, "POST", a.baseURL, bytes.NewReader(body))
    httpReq.Header.Set("x-api-key", a.apiKey)
    httpReq.Header.Set("anthropic-version", "2023-06-01")

    resp, err := a.client.Do(httpReq)
    // ... 读体、看 status、json.Unmarshal 进 CreateMessageResponse
}
```

**三个不那么显而易见的点**：

1. **"零值才填默认值" 模式** —— 只有当调用方留空时，`req.Model` 和 `req.MaxTokens` 才被 provider 自己的默认值填上。这让 `main.go` 保持简洁的同时还允许逐次 override（在 `TestAnthropicProviderModelOverride` 里验证）。
2. **状态码用 `/100 != 2` 而不是 `== 200`** —— Anthropic 今天回 200，但 streaming endpoint 以后可能回 201 / 206，我们希望 s01 这层模式能撑下去。
3. **不重试不退避** —— 这两件事属于 `s14-cost-and-recovery`。在 s01 里"聪明"地加上，会逼后面每一节要么继承这套策略要么单独凿掉。

## What Changed (vs. baseline)

这是第一节 —— 整个 repo 就是 diff。后面每节这一段都会列出 vs. 上一节的具体变化。

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimum-loop

# 默认 model + 默认 system prompt
go run . hello in three words

# 换模型 + 换 system
go run . -model claude-haiku-4-5 -system "Reply in haiku" "what is HTTP"

# 测试不需要 API key（用 httptest）
go test -count=1 ./...
```

## Upstream Source Reading

s01 镜像的机制在 opencode 的 `packages/opencode/src/session/llm.ts` 里：

```ts
// upstream:packages/opencode/src/session/llm.ts#L35-L120 （节选；ai-sdk wrapper）
export const Service = Effect.gen(function* () {
  const provider = yield* Provider.Service
  return {
    stream: (input) => Effect.gen(function* () {
      const model = yield* provider.model(input.providerID, input.modelID)
      return streamText({
        model,
        system: input.system,
        messages: input.messages,
        tools: input.tools,
        toolChoice: input.toolChoice,
        // ... + retry, abort, telemetry
      })
    })
  }
})
```

opencode 的版本走 `Effect.gen`（typed-effect runtime），从一个 Layer 里拉出 `Provider` service，调 Vercel AI SDK 的 `streamText`。我们 s01 把这些全砍了：

- 没有 Effect —— Go 用普通 `func` 和 `error`。
- 没有 `streamText` —— 我们调 `messages.create`（非流式），s06 升级到流式。
- 没有 tools —— s03 / s10 升级。
- 没有 abort signal —— 加流式时 `context.Context` 会隐式带上。

`packages/opencode/src/provider/provider.ts#L87-L150` 是 opencode 把 `(providerID, modelID)` 对解析成具体 `LanguageModelV3` 的地方。`BUNDLED_PROVIDERS` map（line 91）是我们未来 Phase G 里 `provider_openai.go` / `provider_anthropic.go` 的哲学先祖。

opencode LLM 层的阅读顺序：
1. `packages/opencode/src/provider/provider.ts` 1–150 行 —— 接口 + provider map
2. `packages/opencode/src/session/llm.ts` 1–120 行 —— streamText 调用
3. `packages/opencode/src/session/processor.ts` 34–150 行 —— 我们将在 `s10-tool-loop` 重建的 loop

先别再往下读 —— s02–s10 会带你逐层深入。
