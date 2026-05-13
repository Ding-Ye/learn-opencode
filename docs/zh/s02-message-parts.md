---
title: "s02 · 消息与 Part 模型"
chapter: 2
slug: s02-message-parts
est_read_min: 9
---

# s02 · 消息与 Part 模型

> 本章教什么：真实 agent 怎么表示一次 assistant 回合 —— 不是一坨文本，而是一个有序的、带类型的 Part 序列（text + tool_use + tool_result + file + reasoning + ...）。这一节把 union 形状钉准，后面所有节（流式、调度、持久化）都直接在它之上组合，不需要再翻译一次。

---

## Problem

s01 把 assistant 回合建模成 `[]ContentBlock` of text。能打印「hello world」，仅此而已。真实编码 agent 发出的消息长这样：

> 「好，我来读一下那个文件。」 → `tool_use(read, {path:"main.go"})` → `tool_result(...)` → 「看起来 bug 在第 42 行。」

一次 assistant 回合，按顺序携带四个 Part、三种 kind。如果死守 s01 的扁平 `Text string`，要么把所有东西塞进一个超大文本块（丢失工具语义），要么平行开一个 `[]ToolCall` 切片（丢失文本与工具调用之间的相对顺序）。

opencode 在 `packages/opencode/src/session/message-v2.ts` 里这样解：每次 assistant 回合是 `Message{role, parts: Part[]}`，其中 `Part` 是一个 7+ kind 的 tagged union。Go 这边要复刻同款 union —— JSON 对称、向前兼容、switch 一眼能读懂。

## Solution

三步搞定：

1. **`PartKind` 是字符串型枚举**，对齐 Anthropic 的 `type:` 判别字段（`"text"`、`"tool_use"`、`"tool_result"` ...）。Go switch 和 wire 形状共用同一份事实。
2. **`Part` 是带 N 个可选指针的单一 struct** —— `*TextRef`、`*ToolUseRef` 等等。任意合法 Part 必有且只有一个 *Ref 非空。自定义 `MarshalJSON` / `UnmarshalJSON` 在 Go 形状 ↔ 扁平 `{type:..., ...}` wire 形状之间路由。
3. **未知 kind 解码成 `PartUnknown` 并保留原始字节。** opencode 的 wire 格式每个发布版本都新增 variant；拒绝解码 = 上游一发 `compaction` 或 `step-start`，我们的消费方就立刻挂掉。

## How It Works

```
┌──────────────────────────────────────────────────────────────┐
│  s02 Part union                                              │
│                                                              │
│   Go shape                          Wire shape (Anthropic)   │
│   ─────────                         ─────────────────────    │
│   Part{                             {                        │
│     Kind: "text",                     "type": "text",        │
│     Text: &TextRef{                   "text": "hello"        │
│       Text: "hello"                 }                        │
│     }                                                        │
│   }                                                          │
│      │                                  ▲                    │
│      │  MarshalJSON                     │                    │
│      └──── 拼接 "type":kind  ───────────┘                    │
│                                                              │
│      ▲                                  │                    │
│      │  UnmarshalJSON                   │                    │
│      └──── 读 "type"，路由 ◄────────────┘                    │
│            到对应的 *Ref                                     │
└──────────────────────────────────────────────────────────────┘
```

干活的 30 行胶水在 `parts.go`：

```go
type Part struct {
    Kind PartKind

    Text       *TextRef
    ToolUse    *ToolUseRef
    ToolResult *ToolResultRef
    File       *FileRef
    Reasoning  *ReasoningRef
    Snapshot   *SnapshotRef
    Patch      *PatchRef

    Raw json.RawMessage // 未知 kind 的原始字节
}

func (p *Part) UnmarshalJSON(data []byte) error {
    var probe struct{ Type string `json:"type"` }
    if err := json.Unmarshal(data, &probe); err != nil { return err }
    p.Kind = PartKind(probe.Type)

    switch p.Kind {
    case PartText:
        p.Text = new(TextRef)
        return json.Unmarshal(data, p.Text)
    case PartToolUse:
        p.ToolUse = new(ToolUseRef)
        return json.Unmarshal(data, p.ToolUse)
    // ... ToolResult / File / Reasoning / Snapshot / Patch 同模式
    default:
        // 向前兼容：缓存字节，标记 Unknown，不报错。
        p.Kind = PartUnknown
        p.Raw = append(p.Raw[:0], data...)
        return nil
    }
}
```

**三个不显而易见的点**：

1. **wire 扁平、Go 嵌套** —— JSON 是 `{"type":"text","text":"..."}`，**不是** `{"type":"text","data":{"text":"..."}}`。代价是一个 `mergeTyped` 小工具：把 `"type":kind,` 拼到 payload marshal 字节的最前面。备选方案（envelope）会逼后面每一节都先剥一层 envelope —— 14 节复合下来这个税不划算。
2. **tool_result 的 `is_error` 用 `omitempty`** —— 工具成功时 **不能** 发 `"is_error": false`。LLM 把这个字段的 *存在* 当信号，所以阴性 case 必须不出现在 wire 上。`TestRoundtripToolResultPart` 验证了这一点。
3. **`PartUnknown` 重 marshal 出来字节一致** —— 碰到不认识的判别字段时，我们把 `data` 缓存到 `Raw`，`MarshalJSON` 在 `PartUnknown` 分支直接原样返回 `Raw`。这让 encode/decode/encode 在 *结构上不理解* 的输入上也是 fixed-point —— 给 s06 流式层埋的礼物，万一某个未知 event 漏进来不会炸。

## What Changed (vs. s01)

```diff
 type Message struct {
-    Role    string         `json:"role"`
-    Content []ContentBlock `json:"content"`
+    ID      string `json:"id,omitempty"`
+    Role    string `json:"role"`
+    Content []Part `json:"content"`
 }

-// s01 只装文本
-type ContentBlock struct {
-    Type string `json:"type"`
-    Text string `json:"text,omitempty"`
-}
+// Tagged union：每个 Kind 必有且只有一个 *Ref 非空。
+type Part struct {
+    Kind       PartKind
+    Text       *TextRef
+    ToolUse    *ToolUseRef
+    ToolResult *ToolResultRef
+    File       *FileRef
+    Reasoning  *ReasoningRef
+    Snapshot   *SnapshotRef
+    Patch      *PatchRef
+    Raw        json.RawMessage // PartUnknown 的 payload
+}
```

Wire 格式不变：每个 Part 仍序列化为 `{"type":"...", ...}`。所以一个对着 s01 `ContentBlock` 写的 HTTP 层，能不改一行就解码 s02 的 text Part —— 只是消费侧的 switch 多了几条 arm。

## Try It

```bash
cd agents/s02-message-parts

# Demo：构一个 3-part 消息，marshal 出来，再 decode 回去。
go run .

# 5 个测试，无网络。
go test -count=1 ./...

# 看一个 tool_use part 的 wire JSON：
go run . | sed -n '/wire JSON/,/decoded/p' | head -30
```

## Upstream Source Reading

s02 镜像的机制在 opencode 的 `packages/opencode/src/session/message-v2.ts` 第 76–123 行。它定义了 `partBase`（公共的 id/sessionID/messageID）加上我们 Go 侧建模的 7 个 Part variant 中的 4 个。opencode 用 Effect 的 `Schema.Struct` + `Schema.Literal("text")` 表达同款 tagged union；wire JSON 完全一致。

```ts
// upstream:packages/opencode/src/session/message-v2.ts#L76-L123
const partBase = {
  id: PartID,
  sessionID: SessionID,
  messageID: MessageID,
}

export const SnapshotPart = Schema.Struct({
  ...partBase,
  type: Schema.Literal("snapshot"),
  snapshot: Schema.String,
}).annotate({ identifier: "SnapshotPart" })

export const PatchPart = Schema.Struct({
  ...partBase,
  type: Schema.Literal("patch"),
  hash: Schema.String,
  files: Schema.Array(Schema.String),
}).annotate({ identifier: "PatchPart" })

export const TextPart = Schema.Struct({
  ...partBase,
  type: Schema.Literal("text"),
  text: Schema.String,
  synthetic: Schema.optional(Schema.Boolean),
  ignored: Schema.optional(Schema.Boolean),
  time: Schema.optional(
    Schema.Struct({
      start: NonNegativeInt,
      end: Schema.optional(NonNegativeInt),
    }),
  ),
  metadata: Schema.optional(Schema.Record(Schema.String, Schema.Any)),
}).annotate({ identifier: "TextPart" })

export const ReasoningPart = Schema.Struct({
  ...partBase,
  type: Schema.Literal("reasoning"),
  text: Schema.String,
  metadata: Schema.optional(Schema.Record(Schema.String, Schema.Any)),
  time: Schema.Struct({
    start: NonNegativeInt,
    end: Schema.optional(NonNegativeInt),
  }),
}).annotate({ identifier: "ReasoningPart" })
```

Permalink：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-v2.ts#L76-L123>

我们留下了什么、砍了什么：

- **留下** —— `type:` 判别字段、每 variant 一个 struct、JSON 形状。
- **暂时砍掉** —— `partBase`（要等 s07 的 Session 表）、`time` 和 `metadata`（UI / compaction 细节）、`synthetic` 和 `ignored`（后续 compaction 信号）、以及 `ToolPart.state` 状态机（`pending|running|completed|error`）—— 我们用一个扁平 `ToolResultRef` 代替，s10 工具执行循环时再展开。
- **向前兼容** —— opencode 持续新增 variant（`compaction`、`step-start`、`retry`、`agent`、`subtask`...）；我们的 `PartUnknown` 分支让 loop 在每次 opencode 发版时都不需要 Go 这边先升级才能跑。

opencode 消息模型的阅读顺序：
1. `packages/opencode/src/session/message-v2.ts` 76–207 行 —— 所有 Part variant
2. `packages/opencode/src/session/message-v2.ts` 248–320 行 —— `ToolState` + `ToolPart`（s10 的地盘）
3. `packages/opencode/src/session/processor.ts` 34–150 行 —— 消费 Part 流的那一层（s09 的地盘）
