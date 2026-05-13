---
title: "附录 A · Provider 抽象哲学"
chapter: 100
slug: appendix-a-provider-philosophy
est_read_min: 14
---

# 附录 A · Provider 抽象哲学

> 本附录教什么：为什么 s05 的 **Provider** 是 interface 而不是具体类型；这层抽象在防御什么失败模式；TypeScript 那边（Vercel AI SDK + opencode 的 `BUNDLED_PROVIDERS`）是怎么解决同一个问题的；这个模式翻译成 Go 的标准写法长什么样；以及在抽象的接缝处我们丢掉了什么 —— Anthropic 的 prompt-caching 头、OpenAI 的 structured-output 模式之类的厂商专属选项，根本套不进一刀切的接口。这套心智模型让 s05 / s06 / s10 读起来连贯，对未来你写任何 agent 框架也都可复用。

---

## 在防御的失败模式

把某一家 provider 的 HTTP 形状钉死进 agent，是一笔会在某个具体星期二来收账的债：那天 Anthropic 弃用 `messages-2023-06-01`（或者 OpenAI 把 ChatCompletions 切到 v2 schema、或者 Google 把 Gemini 从 `generateContent` 改成 `streamGenerateContent`、或者你公司因合规要求必须接 Bedrock）。那天，你的代码里凡是出现 `claude-3-5-sonnet`、`https://api.anthropic.com/v1/messages`、`x-api-key`、`content_block_delta` 或 `messages.start` 的每一个文件，都得动。

具体到几种债：

- **字符串字面量 `"https://api.anthropic.com/v1/messages"` 出现在 14 个文件里。** URL 一变，你的 test fixture、retry wrapper、usage parser、auth header 逻辑全都要打补丁。它们没有一个需要知道这个 URL —— 它们要的只是「给我一条流式的、类型化 Event 的 response」。
- **SSE parser 是按 Anthropic 帧格式写的。** `messages.start` / `content_block_delta` / `message_stop` 是一种特定协议。OpenAI 的 SSE 是 `data: {choices:[{delta:{content:...}}]}` 加一行 `data: [DONE]`。如果你的 loop 直接读 `event.type === "content_block_delta"`，连「指向另一家厂商」都做不到，得把消费者重写。
- **Tool-use 形状泄漏。** Anthropic 把 `{type: "tool_use", id, name, input}` 作为 content block 发；OpenAI 把 `{tool_calls: [{id, function: {name, arguments}}]}` 作为顶层 message 字段发。如果你的 registry 直接按 Anthropic 形状 dispatch，OpenAI 的转译就只能在错的地方做 —— 每一个调用点 —— 而不是在接缝处一次到位。
- **Auth header 泄漏。** `x-api-key`（Anthropic）vs `Authorization: Bearer`（OpenAI）vs `aws-sigv4`（Bedrock）vs OAuth（Copilot）vs IAM（Vertex）。如果你的 retry / refresh 逻辑伸手进 request 加 header，它就隐式地知道每一家 provider。

四种债的共同根因是 **representation leak**：provider 专属的数据格式（URL、SSE 帧、tool 形状、header）被不该看见的代码看见了。修法是教科书级别的 —— 在表示不同的地方画一条接缝，让接缝后面的实现知道格式即可。

接缝就是 Provider interface。它后面：一个 Anthropic 实现、一个 OpenAI 实现、一个测试用的 fake。它前面：所有其它模块 —— Streaming Loop、Orchestrator、Usage 累加器 —— 看到的是统一的 `Stream(ctx, Request) → Stream<Event>`。等那个星期二真到了，你只改接缝后面的实现。

## TypeScript 这边：Vercel AI SDK 的做法

TypeScript 生态用 Vercel AI SDK（npm 上的 `ai` 包）解决这事。它的核心抽象是 `streamText({ model, system, messages, tools })`。这里 `model` 是个值，不是一条代码路径：

```ts
import { streamText } from "ai";
import { anthropic } from "@ai-sdk/anthropic";
import { openai } from "@ai-sdk/openai";

// 调用点完全不知道 provider：
const { textStream, toolCalls } = await streamText({
  model: anthropic("claude-3-5-sonnet"),  // 或 openai("gpt-4o")
  system: "You are helpful.",
  messages: [{ role: "user", content: "hello" }],
  tools: { /* ... */ },
});
```

两条设计决策在干所有的活：

1. **`model` 是个 `LanguageModelV3` 值，不是字符串、也不是被继承的 class。** `anthropic(...)` 是工厂函数，返回一个满足 `LanguageModelV3` interface 的对象（`doStream`、`doGenerate`、能力标记）。SDK 从不去类型 switch 这个值是哪家 provider 造的；它只调 `model.doStream(prompt)`，相信实现。
2. **所有 provider 归一化到同一种 Event 协议。** 不管线上是 Anthropic 的 `content_block_delta` 还是 OpenAI 的 `data: {choices:[...]}`，跨过接缝时它就是 `LanguageModelV3StreamPart` —— text-delta、tool-call-delta、tool-call、finish、error 等。帧格式差异被埋在 `@ai-sdk/openai` 和 `@ai-sdk/anthropic` 各自的适配器里。

这就是「provider 是数据，不是代码路径」 —— 调用点持有一个接口类型的值、通过它 dispatch。代价是真正属于厂商专属的东西（Anthropic prompt-caching 头、OpenAI 的 `response_format: {type: "json_schema"}`、Bedrock guardrails）要么塞进 interface（一般是 `providerOptions: { anthropic?: {...}, openai?: {...} }`）、要么走旁路。我们在第 5 节回到这一点。

## opencode 的 `BUNDLED_PROVIDERS` map

opencode 在 Vercel SDK 之上再包了一个 registry：把字符串 provider ID 解析成工厂函数。整个东西就一个 map：

```ts
// packages/opencode/src/provider/provider.ts L91–L117
const BUNDLED_PROVIDERS: Record<string, () => Promise<(opts: any) => BundledSDK>> = {
  "@ai-sdk/amazon-bedrock": () => import("@ai-sdk/amazon-bedrock").then((m) => m.createAmazonBedrock),
  "@ai-sdk/anthropic": () => import("@ai-sdk/anthropic").then((m) => m.createAnthropic),
  "@ai-sdk/azure": () => import("@ai-sdk/azure").then((m) => m.createAzure),
  "@ai-sdk/google": () => import("@ai-sdk/google").then((m) => m.createGoogleGenerativeAI),
  "@ai-sdk/google-vertex": () => import("@ai-sdk/google-vertex").then((m) => m.createVertex),
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
};
```

三件值得注意：

- **value 是 lazy thunk**（`() => import(...)`）。`import(...)` 是动态 import，所以从不设 `provider: "groq"` 的用户也不会为 Groq 的 bundle 体积买单。Go 没有这个机制 —— 每个 provider 都无条件编进 binary —— 但 Go 也没有 25 家 provider 都打包的体积问题。
- **key 是 npm 包名，不是友好 ID。** 因为同一个 map 在 opencode 的 logging / auth 里既要做「加载什么」又要做「记账给谁」。改成更友好的 `"anthropic" → loader` map 是显然的重构；opencode 选了自描述 key。
- **factory 返回的是 `(opts: any) => BundledSDK` thunk**，不是 SDK 本身。第二层间接是为了把 API key 和按调用绑定的选项绑进去。在 Go 我们用 constructor 表示：`func NewAnthropic(opts AnthropicOpts) Provider`。

map 周边的机制平淡无奇：`provider.ts` 拿 `(providerID, modelID)` 对，查 loader、await 它、用 auth options 调它、然后向得到的 SDK 要 `languageModel(modelID)`。外层代码持着这个 `LanguageModelV3` 值传给 `streamText`。

## Go 的标准翻译

Go 没有动态 import、也没有 TypeScript 的结构化类型。翻译过来是这样的：

1. **在 s05 把 `Provider` 定义成 interface。**

   ```go
   type Provider interface {
       Stream(ctx context.Context, req Request) (Stream, error)
   }
   ```

2. **把 `Stream` 和 `Event` 也定义成 interface + 标签联合。**

   ```go
   type Stream interface {
       Next() (Event, error)   // 结束时返回 ErrStreamDone
       Close() error
   }

   type Event struct {
       Type      EventType   // EventText | EventToolUse | EventFinish | …
       Text      string
       ToolUse   *ToolUse
       Reasoning string
       Usage     *Usage
   }
   ```

3. **每个 provider 占一个文件。** s05 出 `AnthropicProvider`。未来某个 addendum 会加 `OpenAIProvider` 在 `provider_openai.go`。每个 constructor 是普通 Go 函数，闭包持着 API key 和 base URL。

4. **在一张 map 里注册 constructors（可以放 package init，也可以就放 `main`）。**

   ```go
   var Providers = map[string]func(opts ProviderOpts) Provider{
       "anthropic": NewAnthropicProvider,
       // "openai": NewOpenAIProvider,  // Phase G
   }
   ```

   这张 map 在 Go 里是编译期建的，不是 `import()`。这是个权衡：读起来更简单（没有 async loader），但每个 provider 不管你用不用都在 binary 里。教学仓只跑一两个 provider 不是问题；想出 25 家 provider 的工具就要紧。

5. **在 `main` 拼装**：读 config、按 ID 查 constructor、构造 Provider、传给 Orchestrator (s10)。Orchestrator 的签名只提到 interface —— `func Run(ctx context.Context, p Provider, r *Registry, msg Message) ([]Message, error)`。它不可能不小心知道 Anthropic。

形状上和 TypeScript 的差异：

- **没有按字符串 key 做动态 dispatch。** Go 的静态 dispatch interface 就是 `LanguageModelV3` 的静态等价。map 只用来 *构造* provider，不用来 *使用* provider。
- **没有结构化类型。** 任何号称是 `Provider` 的东西必须显式满足 interface；编译器强制。TS 那边只要满足 `LanguageModelV3` 形状就行，不需要 `implements`。
- **没有 lazy import。** 所有 provider 都被链进来。我们用 binary 体积换 build-time 保证。

interface 本身故意做得小。加方法很贵（每个已存在的实现都得跟着长）。给 `Request` 和 `Event` 加字段很便宜（已存在的实现忽略不认得的字段）。当你想加 `func (p Provider) AnthropicCacheHeader() string` —— 别加。给 `Request` 加一个可选字段、文档里标 Anthropic-only、让其它 provider 忽略它。Vercel SDK 用 `providerOptions` 干的就是这件事。

## 失去的：编译期的 provider 专属选项

「统一 interface」的代价是真正不一样的 provider 特性失去了类型化、IDE 可发现的家。三个具体例子：

**1. `req.Tools` 形状分歧。** Anthropic 的 tool 格式是 `{name, description, input_schema}`。OpenAI 的是 `{type: "function", function: {name, description, parameters}}`。Google Gemini 是 `{name, description, parameters}` 加一层 function-declarations 包装。s05 的 interface 里 `Request.Tools []ToolSchema` 只载得动一种形状（我们选了 Anthropic，因为它是默认）。等 `OpenAIProvider.Stream` 实现时，它得在自己函数体里把 `[]ToolSchema` *翻译* 成 OpenAI 形状。没有编译期检查这个翻译处理了每个字段；比如 Anthropic 形状的 `cache_control` 字段在 OpenAI 那边没有对应物，被悄悄丢掉。

   Vercel SDK 用 **canonical 内部 tool 格式**（`LanguageModelV3FunctionTool`）解决：每个适配器把别家形状翻译 *进来*。opencode 继承了这点 —— 调用点从不见到 provider 专属的 tool JSON。我们在 Go 里模拟同样的事：`Request.Tools` 是 canonical 形状，每个 Provider 实现往外翻译。

**2. 可选 reasoning 参数。** Anthropic 的 extended-thinking 模型接受 `thinking: {type: "enabled", budget_tokens: 10000}` 来开 reasoning blocks。OpenAI 的 `o1`/`o3` 模型默认就在做 reasoning（client 不需要 opt-in）。Google Gemini 有 `thinkingBudget`。统一 `Request` 里这变成：

   - 要么 **类型化字段**（`Request.ReasoningBudget int`），有些 provider 听、有些忽略 —— IDE 能发现，语义不可靠。
   - 要么 **provider-options 包**（`Request.ProviderOptions map[string]json.RawMessage`）—— 类型擦除，可靠，但 IDE 发现不了。

   Vercel SDK 选了包（`providerOptions: { anthropic: {...}, openai: {...} }`）。Go 教学仓我们推荐折中：≥2 家 provider 都支持的进类型化字段，纯厂商专属的进包。

**3. Anthropic 的 prompt-caching 头 vs OpenAI 没有。** Anthropic 用 request body 里的 `cache_control: {type: "ephemeral"}` block 加 beta header `anthropic-beta: prompt-caching-2024-07-31` 支持缓存前缀。OpenAI 没有对应的用户面 API（缓存是服务端自动的）。如果 `Request` 暴露 `EnablePromptCaching bool`，它在 OpenAI 上就是个谎言。诚实的 API 是按 Part 的 metadata —— `Part.Metadata["cache"]` —— Anthropic provider 读、OpenAI provider 忽略。这丑但诚实。

总结：**抽象的 provider 越多，你的 interface 越退化成最小公倍数形状，越多的 provider 专属能力被推进无类型逃生口。** Vercel SDK 用 `providerOptions` 明确接受这权衡。opencode 继承之。learn-opencode s05 在编译期只 Anthropic 一家，避开这个问题；未来 addendum 加第二家 provider 时，我们就要同样的逃生口。

## 得到的：fake Provider 带来的可测试性

所有这些痛苦的另一面是 agent 里最干净的东西：**测试用 fake provider**。因为每个消费 LLM 输出的代码都说 `Provider` / `Stream` / `Event` 这套话，测试代码可以塞一个手写脚本的 Provider，吐预设的 Event 序列，零 HTTP、零 API key、零 rate limit、零 flake。

s06 在 `agents/s06-streaming-loop/fake_provider.go` 出的就是这个。形状是：

```go
type FakeProvider struct {
    Events [][]Event   // 每次 Stream() 调用对应一个 slice
    call   int
}

func (f *FakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
    if f.call >= len(f.Events) {
        return nil, fmt.Errorf("fake provider exhausted")
    }
    s := &fakeStream{events: f.Events[f.call]}
    f.call++
    return s, nil
}
```

测试就照着自己想跑的事件序列写：

```go
fp := &FakeProvider{Events: [][]Event{
    {
        {Type: EventText, Text: "I'll read the file."},
        {Type: EventToolUse, ToolUse: &ToolUse{ID: "t1", Name: "read", Input: ...}},
        {Type: EventFinish, Usage: &Usage{InputTokens: 100, OutputTokens: 30}},
    },
    {
        {Type: EventText, Text: "Here is what I found."},
        {Type: EventFinish, Usage: &Usage{InputTokens: 150, OutputTokens: 20}},
    },
}}
```

s10 的 `loop_test.go` 就建在这上面：脚本一段两轮对话（assistant 调 tool、收 result、再完成），把 orchestrator 跑在 fake 上、断言最终消息切片正好有期望的 parts。无网络、无 API key、确定性、跑一次不到一毫秒。

「测试快」之外这买到了两件东西：

- **对抗场景容易写。** Fake 可以吐一个 target 被权限拒绝的 `EventToolUse`，断言 orchestrator 在 result Part 上返回 `IsError: true`（而不是 panic、不是 crash）。Fake 可以中途吐 `context_length_exceeded`，断言 s14 分类器返回 `ShouldCompact == true`。Fake 可以坚决不结束，断言 `MaxIterations` 真的把 loop 卡住了。
- **被测的是 interface，不只是某个实现。** 每一个跑在 `FakeProvider` 上的测试都是「orchestrator（以及 loop、registry、permission、usage）说的是 interface 不是 Anthropic 实现」的保证。`OpenAIProvider` 加进来那天，所有这些测试不动一行就还过 —— 因为它们从一开始就没依赖 Anthropic。

这是第 1 节那条接缝纪律的实用回报：它不只是为了挺过 Anthropic 弃用某个 endpoint 的那一天。它是为了让 agent 根本能被测 —— 不付 token、不 flake、不 mock HTTP。Interface 驱动的设计在每一次跑测、每一次 CI build、每一次重构里都在分红 —— 不只是稀有的「线协议迁移日」那天。
