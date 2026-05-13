---
title: "s14 · 成本与错误恢复"
chapter: 14
slug: s14-cost-and-recovery
est_read_min: 11
---

# s14 · 成本与错误恢复

> 本章教什么：每一次 LLM 请求都会回吐 token 计数，也可能以三种本质不同的方式失败。s14 在这两件事上加两条操作层：一个 `Usage` 累加器（带 `Add` / `TotalCost`），让 session 行始终反映**整段对话**的累计花费；以及一个 `WithRetry` 包装器加上四种错误类型（`APIError` / `AuthError` / `ContextOverflowError` / `AbortedError`），让 429 退避重试、auth 错误立即放弃、context overflow 触发 compaction 信号、Ctrl-C 立刻打断退避 sleep。

---

## Problem

s07 给了 session 行用来持久化；s10 接好了 streaming tool loop。还剩两条任何真实 agent 用户上线前必须补上的操作短板：

- **Cost 看不见。** 每次 Anthropic 请求会返回一个 `usage` 对象，里面是五个整数（input、output、reasoning、cache.read、cache.write）。如果不把它们累加到 session 行上，就回答不了「这段对话花了多少钱？」—— 更糟的是没法实时显示累计开销，让用户在跑飞的 loop 把预算抽干前还有机会停手。
- **失败种类不一样。** 429（限流）是临时的：等等再试。503（网关超时）是临时的：等等再试。401（API key 错）是永久的：用同一个坏凭证再试还是同样错。`context_length_exceeded` 错说明对话超了模型窗口，恢复办法是**总结后重发**，**不是**重试。用户按 Ctrl-C 是要立刻停，**不是**等下一个退避结束。一个把所有错误都用 `if err != nil { retry() }` 接住的 agent，会拿 auth 错误烧预算、对用户的取消视而不见。

没有这两条层时 agent 会踩到的真实痛点：

- 长跑 loop 撞到一个 429 就崩了，用户得手动重新提问。退避重试本来能透明恢复。
- API key 过期了，触发一波重试风暴 —— 600ms 内连续三个 401 —— 某些供应商会把这种行为标成可疑。
- 对话累到 200k token，下一次请求返回 `context_length_exceeded`，loop 重试三次后才放弃。这三次每次都用同一份太长的 payload，注定会同样失败；唯一的修法是 compaction。
- 用户在一次 5 秒退避 sleep 时按了 Ctrl-C。他们期待立刻退出，结果等到 timer 走完才结束。

opencode 的答案是**在 transport 和业务逻辑的接缝处分类错误，只对真正可重试的重试，对超长 payload 触发 compaction 信号** —— 同时把每次请求的 `usage` 累加到 session 行上，让累计开销随时一列可读。

s14 搭这条 cost-and-recovery 接缝。它**不**搭：

- compaction 例程本身（上游约 400 行的 `Session.compact()` 总结旧消息 —— 不在范围；s14 只发**信号** `ShouldCompact(err) == true`）。
- Decimal 精度计费（用 float64；上游用 Decimal.js；教学场景下 float64 的 ~15 位有效数字够用）。
- 从 provider 价格目录读实时定价（我们硬编两个模型的 MOCK 常量；上游 opencode 在请求时从 AI SDK 取价）。
- 完整的 `@ai-sdk` APIError 形状（responseHeaders、requestBodyValues、错误对象自带的 `isRetryable` 标志）；我们只留 StatusCode + Body，让 `IsRetryable` 从 status code 推断。

## Solution

一个 struct、三个函数、四种错误类型，每条恢复路径一个分类器：

```go
type Usage struct {
    InputTokens, OutputTokens, ReasoningTokens int
    CacheReadTokens, CacheWriteTokens          int
}
func (u *Usage) Add(other Usage)
func (u Usage) TotalCost(pricing Pricing) float64

type APIError struct { StatusCode int; Body string }   // transport / 服务端
type AuthError struct { Provider string }              // 凭证错
type ContextOverflowError struct { CurrentTokens, ModelLimit int }
type AbortedError struct{}
func IsRetryable(err error) bool                        // 分类器 1
func ShouldCompact(err error) bool                      // 分类器 2

type RetryPolicy struct { MaxAttempts int; BaseBackoff, MaxBackoff time.Duration }
func WithRetry(ctx context.Context, p RetryPolicy, op func() error) error
```

各自做的事：

- **`Usage.Add`**：把 `other` 原地加到 receiver 上。每次 Provider.Stream 完成后调用 —— 这一轮的 usage 累加到 session 全局 Usage 上，持久化的行始终反映**整段**对话，而不是只有最后一轮。
- **`Usage.TotalCost`**：在给定 Pricing 下的美元开销。Reasoning token 按 OUTPUT 费率计费（对齐 Anthropic 政策），所以我们把 reasoning 折进 output 项里，而不是单开一个 `ReasoningPerMTok` 字段。
- **`IsRetryable`**：仅对 `*APIError` 状态码 429 或 >= 500 返回 true。其他一切（AuthError、ContextOverflowError、AbortedError、429 以外的 4xx、stdlib 错误）一律返回 false。重试 AuthError 会踩坑；重试 ContextOverflowError 注定失败。
- **`ShouldCompact`**：仅对 `*ContextOverflowError` 返回 true。context overflow 的恢复跟 retry 本质不同 —— 它**会改变** request payload（丢掉或总结消息）。两种恢复，两个谓词；它们不共享返回值。
- **`WithRetry`**：跑 op，分类它的错误，按指数退避（capped 在 `MaxBackoff`）sleep，最多重试 `MaxAttempts` 次。三条核心规则：先分类再重试、sleep 时尊重 ctx.Done()、所有 attempt 都失败时返回**最后一次**错误（不是第一次）。

**为什么两个分类器，不是一个三态枚举**：可以写成 `Classify(err) → Retry | Compact | Fatal`，但调用点不同。Retry 在 `WithRetry` 内部用；compaction 在 s10 外层 loop 在 `WithRetry` 返回之后用。两个层级各有一个单一用途的函数，比一个三分支函数在两个地方各消费一遍要清楚。

**为什么 Usage struct 把 cache 拍平**：上游用 `tokens.cache.{read,write}` 嵌套是为了对齐 Anthropic API 响应形状。在 Go 里没有理由把两个整数嵌套；拍平成 `CacheReadTokens` + `CacheWriteTokens` 读起来更顺，也符合 Go 偏好浅 struct 的风格。算账（TotalCost）逻辑相同。

**为什么用 MOCK 定价常量**：真正的 opencode 在请求时从 AI SDK 的 provider 目录拉实时费率（费率会变，新模型会出现）。教学场景下硬编常量保证 demo deterministic、test 稳定。每个常量上的 `// MOCK` 注释是一个核心警告：任何想把它复制进计费 pipeline 的人都该看到。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s14 cost + recovery                                                   │
│                                                                        │
│  ── cost ─────────────────────────────────────────────────             │
│   Usage{InputTokens: 1200, OutputTokens: 350,                          │
│         ReasoningTokens: 80, CacheReadTokens: 10000,                   │
│         CacheWriteTokens: 200}                                         │
│        ↓ Add(turn2)                                                    │
│   Usage{... 累加后的五个整数 ...}                                       │
│        ↓ TotalCost(PricingClaudeSonnet4_5)                             │
│   $0.026700  （reasoning 按 OUTPUT 费率算）                             │
│                                                                        │
│  ── recovery ─────────────────────────────────────────────             │
│   WithRetry(ctx, p, op):                                               │
│     for attempt = 0 .. p.MaxAttempts-1:                                │
│       if ctx.Err() != nil { return ctx.Err() }     ← cancel 优先        │
│       err = op()                                                       │
│       if err == nil { return nil }                                     │
│       if !IsRetryable(err) { return err }          ← auth/ctx 立刻退    │
│       if attempt == last { break }                                     │
│       select {                                                         │
│         case <-ctx.Done(): return ctx.Err()        ← cancel 优先        │
│         case <-time.After(backoff):                ← sleep             │
│       }                                                                │
│       backoff = min(backoff*2, p.MaxBackoff)       ← 指数封顶           │
│     return lastErr                                 ← 最后一次，不是第一次 │
│                                                                        │
│   IsRetryable:    APIError{429} | APIError{>=500}  → true              │
│                   其他一切                          → false             │
│   ShouldCompact:  ContextOverflowError             → true              │
│                   其他一切                          → false             │
│                                                                        │
│  ── 外层 (s10's loop) ─────────────────────────────                    │
│   err := WithRetry(ctx, policy, func() error {                         │
│       stream, err := provider.Stream(ctx, req)                         │
│       if err != nil { return err }                                     │
│       sessionUsage.Add(consume(stream))            ← 累加               │
│       return nil                                                       │
│   })                                                                   │
│   switch {                                                             │
│   case ShouldCompact(err):  compactAndRetry(...)   ← context overflow  │
│   case errors.As(err, &authErr):  promptReauth()   ← auth              │
│   case err != nil:          surfaceToUser(err)     ← 放弃               │
│   }                                                                    │
└────────────────────────────────────────────────────────────────────────┘
```

**五个核心决策**：

1. **先分类再重试。** `WithRetry` 在 sleep 前先 `IsRetryable(err)`。AuthError 和 ContextOverflowError 永远不会 sleep，它们在 attempt 0 就返回。这是**安全属性**（不是单纯优化）：因为重试 AuthError 可能触发账号锁定，重试 ContextOverflowError 拿一份注定失败的 payload 烧预算。
2. **ctx 优先于退避。** sleep 循环里的 select 有两个 case：timer 触发，以及 ctx.Done()。两者都 ready 时 Go 的 select 是伪随机选的 —— 但实际中 ctx 总是赢，因为只要它触发了，每次迭代顶部的 cancel 检查也会接住。`TestWithRetryRespectsContextCancel` 把这条钉住。
3. **最后一次错误胜出。** 全部 attempt 都失败时，返回**最后一次** `err`，不是第一次。最近一次服务端响应通常最有信息量（一个临时 503 后跟一个永久 503 应该把永久那个露出来）。对应上游的 `lastError` 累加器模式。
4. **两个分类器，不是一个 switch。** `IsRetryable` 和 `ShouldCompact` 故意分开。把它们合并会让一个 context-overflow 错误被意外重试 —— 在某人发现 payload 没变之前。
5. **Reasoning 按 output 计费。** `TotalCost` 把 `OutputTokens + ReasoningTokens` 加起来再乘 output 费率。Anthropic 把 reasoning 收的费率跟 output 一样；折成一项与账单对齐，也不必带一个永远等于 `OutputPerMTok` 的 `ReasoningPerMTok` 字段。

**为什么大约 400 LOC（含测试）**：因为这事本来就小。Usage 是五个 int 加一个 `Add` 加一次乘法；errors 是四个类型加两个分类函数；retry 是一个 for 循环加一个 select。5 个测试探到每条分支。没有 goroutine、没有 channel（timer 是唯一异步面）、没有 I/O —— Go 标准库就够。

## What Changed (vs. s10/s11)

s10 接好了 streaming tool loop：`for !done { stream → 派发 tool → 追加结果 → 再调 provider }`。s11 在 s08 的 config 层之上加了 skill 发现。s14 在 s10 的 loop 外**包**一层新操作层：

```diff
 // s10：loop 内裸调 provider。
 stream, err := provider.Stream(ctx, req)
 if err != nil {
-    return fmt.Errorf("provider stream: %w", err)
+    return fmt.Errorf("provider stream: %w", err)  // 不分类、单次 attempt
 }

+// s14：把每一轮的 provider 调用包进 WithRetry；累加 usage。
+var turnUsage Usage
+err := WithRetry(ctx, DefaultRetryPolicy(), func() error {
+    stream, err := provider.Stream(ctx, req)
+    if err != nil {
+        return err
+    }
+    turnUsage = consume(stream)  // 从 stream events 抓本轮 usage
+    return nil
+})
+session.Usage.Add(turnUsage)
+session.Cost = session.Usage.TotalCost(modelPricing)
+
+switch {
+case ShouldCompact(err):
+    // 丢或总结旧消息，然后重发请求。
+    req = compact(req)
+    continue
+case errors.As(err, &authErr):
+    return fmt.Errorf("re-authenticate: %w", err)
+case err != nil:
+    return err
+}
```

diff 里核心的事：s10 的 loop 形状没变（同样的外层 for、同样的 provider.Stream 调用）。s14 **加**了一个 wrapper（WithRetry）和一段 loop 后 usage 累加。这种解耦是刻意的 —— s10 的契约（「轮 → stream → tools → 重复」）跟 s14 的契约（「分类 → 重试 → 累加」）是独立的，所以可以分别测。

s12（MCP）和 s13（LSP）将做的事：同样的模式 —— 给注册表加 tool，不动 loop 形状。s14 的 WithRetry 透明地包住任意一种，因为分类器读的是错误类型，不是调用点。

## Try It

```bash
cd agents/s14-cost-and-recovery

# Demo（deterministic，无网络）：
go run .

# 5 个测试：
go test -count=1 ./...

# vet + build + test 一起跑：
go vet ./... && go build ./... && go test -count=1 ./...
```

5 个测试覆盖：

1. **TestUsageAddAccumulates** —— 五个 token 字段（Input、Output、Reasoning、CacheRead、CacheWrite）每一个都跨多次 `Add` 正确累加；最终 `TotalCost` 为正（防止 Add 写成覆盖而非累加的回归）。把计费的核心算术钉住。
2. **TestWithRetryGivesUpAfterMaxAttempts** —— 当 op 一直返回 APIError(429) 时，WithRetry 恰好调 op `MaxAttempts` 次然后返回**最后一次**错误（不是第一次、不是 nil）。同时钉住调用次数和返回错误类型。
3. **TestWithRetryDoesNotRetryAuthError** —— AuthError 立即返回；op 恰好调一次。这是安全属性 —— 重试 auth 在某些供应商会触发账号锁定。
4. **TestShouldCompactOnlyForContextOverflow** —— 表驱动测试覆盖 7 种错误变体（ContextOverflowError、APIError 429、APIError 500、AuthError、AbortedError、stdlib 错、nil）。只有 ContextOverflowError 返回 true。把「compaction 永远不在错的错误上触发」契约钉住。
5. **TestWithRetryRespectsContextCancel** —— 当 ctx 在退避 sleep 期间触发时，WithRetry 在远低于跑完 MaxAttempts 所需时间的时间内返回 `ctx.Err()`。把「ctx 优先于退避」钉住，Ctrl-C 才能即时。

## Upstream Source Reading

s14 对应两个上游文件：`packages/opencode/src/session/session.ts` L91-L142 是 cost / Usage 形状，`packages/opencode/src/session/message-error.ts` L1-L14 是错误分类。完整的 session.ts 有 1000+ 行，覆盖 Session 行的每一列；我们只摘 cost 相关那段。message-error.ts 总共 14 行 —— 我们逐行注释，因为整个文件**就是**分类。

```ts
// upstream:packages/opencode/src/session/session.ts L91-L142

// L91-L98 —— fromRow 内的 tokens 形状。五个整数加一个嵌套 `cache` 子对象。
// 我们的 Go Usage 把 cache 拍平（CacheReadTokens + CacheWriteTokens），
// 因为没有第三个 cache 字段值得嵌套。
return {
  // ... id、slug、projectID 等在前 ...
  cost: row.cost,                                       // ★ Decimal 精度的美元
  tokens: {                                              // ★ 五整数记录
    input: row.tokens_input,                             //   provider input 费率
    output: row.tokens_output,                           //   output 费率
    reasoning: row.tokens_reasoning,                     //   ★ 也按 OUTPUT 费率计费
    cache: {                                             //   嵌套对齐 Anthropic API
      read: row.tokens_cache_read,                       //   重折扣
      write: row.tokens_cache_write,                     //   比 input 略贵
    },
  },
  // ... share、revert、permission、time 等在后 ...
}

// L112-L142 —— toRow：反过来，把 Usage 持久化到 SQLite。
// L131-L135 带着核心的「null 时落到 EmptyTokens」模式。Go 这边白拿：
// 零值的 Usage 就是空 token 记录 —— 不需要单独的 sentinel。
export function toRow(info: Info) {
  return {
    // ... id、project_id 等在前 ...
    cost: info.cost ?? 0,                                // ★ undefined 时默认 0
    tokens_input: (info.tokens ?? EmptyTokens).input,    // ★ 拍平成 SQL 行上的
    tokens_output: (info.tokens ?? EmptyTokens).output,  //   五个独立列
    tokens_reasoning: (info.tokens ?? EmptyTokens).reasoning,
    tokens_cache_read: (info.tokens ?? EmptyTokens).cache.read,
    tokens_cache_write: (info.tokens ?? EmptyTokens).cache.write,
    // ... revert、permission、time_* 等在后 ...
  }
}

// upstream:packages/opencode/src/session/message-error.ts L1-L14

import { Schema } from "effect"
import { NamedError } from "@opencode-ai/core/util/error"

// L4 —— OUTPUT 侧溢出。我们的 Go ContextOverflowError 把这个和 input
// 侧那个合并，因为恢复办法相同：COMPACT 后重发。ShouldCompact 对两者
// 都返回 true。
export const OutputLengthError = NamedError.create("MessageOutputLengthError", {})

// L6-L9 —— 凭证错。带 providerID 让 UI 能说「你的 Anthropic key 看起来
// 不对」而不是单单「auth 失败」。这是「不要重试」的标杆错误。
export const AuthError = NamedError.create("ProviderAuthError", {
  providerID: Schema.String,
  message: Schema.String,
})

// L11-L12 —— 下游分派依靠的 union。NamedError.Unknown.EffectSchema 是
// 兜底（在 Go：任何 errors.As 不匹配我们 typed sentinel 的错误）。
export const Shared = [
  AuthError.EffectSchema,
  NamedError.Unknown.EffectSchema,
  OutputLengthError.EffectSchema,
] as const
export const SharedSchema = Schema.Union(Shared)
```

逐行批注（关键行）：

- **session.ts L91-L98 tokens 形状** —— 五个整数加嵌套 cache。我们的 Go Usage 留五个整数（拍平），用 zero-value 语义处理「没命中 cache」的情况，省了 NullableCache 包装。
- **session.ts L94 reasoning** —— 按 OUTPUT 费率（Anthropic 政策）。我们的 `TotalCost` 把 reasoning 折进 output 项跟账单对齐；不带永远等于 `OutputPerMTok` 的独立 `ReasoningPerMTok`。
- **session.ts L130 `info.cost ?? 0`** —— 「默认 0」模式。Go 里未初始化的 `float64` 字段**就是** 0；不需要 `?? 0`。同理也省掉五个 token 字段的兜底。
- **session.ts L131-L135 `?? EmptyTokens`** —— null-safe 访问模式。Go 的 zero-value 语义意味着新分配的 `Usage{}` 五个字段已经全是 0；不需要 `EmptyTokens` 常量。
- **message-error.ts L4 OutputLengthError** —— output 侧溢出，跟 input 侧 context overflow 在上游是不同类。我们的 Go 把两者并入 `ContextOverflowError`，因为恢复（compaction）不在乎是哪侧溢的。
- **message-error.ts L6-L9 AuthError** —— 「不要重试」的标杆 sentinel。我们的 Go `*AuthError` 形状一样；`IsRetryable` 对它返回 false。
- **message-error.ts L11-L12 Schema.Union** —— Effect 的运行时检查分派面。我们的 Go 用分类器内部的 `errors.As` 替换，编译期类型安全弱一些，但读起来更地道。

Permalink：

- session.ts L91-L98（fromRow 里的 tokens 形状）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L91-L98>
- session.ts L112-L142（toRow 带 cost+tokens）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L112-L142>
- message-error.ts L1-L14（整个错误分类）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-error.ts#L1-L14>

我们留下了什么、砍了什么：

- **留下** —— 五整数 token 形状、reasoning-按-output-计费、AuthError sentinel、「context overflow 需要 compaction」信号、「MaxAttempts 后放弃」上限。
- **暂时砍掉** —— Decimal.js 精度（float64 ~15 位有效数字够用；要做发票级精度就换 shopspring/decimal）、Effect-typed Schema.Union 分派（Go 的 errors.As 等价）、从 AI SDK provider 目录拉实时定价（我们硬编 MOCK 常量）、完整 ai-SDK APIError 形状（responseHeaders、requestBodyValues、错误自带的 `isRetryable` 布尔；我们从 StatusCode 推）、compaction 例程本身（不在范围；上游约 400 行）。
- **向前兼容** —— 给 `Usage` 加第六个 token 字段（比如 `BatchTokens`）很机械：扩 struct、Add、TotalCost。加新错误 sentinel（比如把 `*RateLimitError` 跟 APIError 429 区分）也一样：定义类型，给 IsRetryable 加分支。分类器函数模式可扩展。

opencode cost+recovery 阅读顺序：

1. `packages/opencode/src/session/session.ts` L91-L98 —— `fromRow` 里的 tokens 形状（s14 Usage struct 的祖先）。
2. `packages/opencode/src/session/session.ts` L112-L142 —— `toRow` 带 cost + tokens 列（s14 持久化目标）。
3. `packages/opencode/src/session/message-error.ts` L1-L14 —— 全部错误分类（s14 错误 sentinel）。
4. `packages/opencode/src/session/llm.ts` ~L100-L150 —— streamText 内联的退避重试（s14 把它抽成 WithRetry）。
5. `packages/opencode/src/session/processor.ts` ~L34-L150 —— `ShouldCompact == true` 触发 `Session.compact()` 的地方（s14 的 compaction 信号消费方；compaction 例程本身不在范围）。
