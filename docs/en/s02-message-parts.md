---
title: "s02 · Messages and Parts"
chapter: 2
slug: s02-message-parts
est_read_min: 9
---

# s02 · Messages and Parts

> What this teaches: how a real agent represents a single assistant turn — not as one text blob, but as an ordered sequence of typed Parts (text + tool_use + tool_result + file + reasoning + ...). Get the union shape right here, and every later session — streaming, dispatch, persistence — composes over it without re-translating.

---

## Problem

s01 modeled an assistant turn as `[]ContentBlock` of text. That's enough to print "hello world", and nothing more. A real coding agent emits messages like:

> "Sure, let me check that file." → `tool_use(read, {path:"main.go"})` → `tool_result(...)` → "Looks like the bug is on line 42."

That single assistant turn carries four Parts of three different kinds, in order. If we keep s01's flat `Text string` model, we'd have to either (a) cram everything into one giant text blob (losing tool semantics), or (b) invent a parallel `[]ToolCall` slice (losing ordering vs the prose).

opencode solved this in `packages/opencode/src/session/message-v2.ts`: every assistant turn is `Message{role, parts: Part[]}` where `Part` is a tagged union over 7+ kinds. We need the same union in Go — JSON-symmetric, forward-compatible, and obvious to switch on.

## Solution

Three moves and we're done:

1. **`PartKind` is a string-typed enum** matching Anthropic's `type:` discriminator (`"text"`, `"tool_use"`, `"tool_result"`, ...). One source of truth for both the Go switch and the wire shape.
2. **`Part` is one struct with N optional pointers** — `*TextRef`, `*ToolUseRef`, etc. Exactly one is non-nil for any well-formed Part. Custom `MarshalJSON` / `UnmarshalJSON` route between this Go shape and the flat `{type:..., ...}` wire shape.
3. **Unknown kinds decode as `PartUnknown` with original bytes preserved.** opencode's wire format gains new variants every release; refusing to decode would brick our consumer the moment upstream ships `compaction` or `step-start`.

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
│      └──── splices "type":kind  ────────┘                    │
│                                                              │
│      ▲                                  │                    │
│      │  UnmarshalJSON                   │                    │
│      └──── reads "type", routes ◄───────┘                    │
│            into matching *Ref                                │
└──────────────────────────────────────────────────────────────┘
```

The 30 lines of glue live in `parts.go`:

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

    Raw json.RawMessage // unknown kinds — preserved bytes
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
    // ... same pattern for ToolResult, File, Reasoning, Snapshot, Patch
    default:
        // forward-compat: stash bytes, mark as Unknown, don't error.
        p.Kind = PartUnknown
        p.Raw = append(p.Raw[:0], data...)
        return nil
    }
}
```

**Three non-obvious points**:

1. **Flat wire, nested Go** — The JSON is `{"type":"text","text":"..."}`, NOT `{"type":"text","data":{"text":"..."}}`. We pay for that with a `mergeTyped` helper that splices `"type":kind,` into the front of the payload's marshaled bytes. The alternative (envelope) would force every later session to peel the envelope before reading the payload — a tax compounded across 14 sessions.
2. **`is_error` on tool_result is `omitempty`** — A tool that succeeds must not emit `"is_error": false`. The LLM treats *presence* of the field as a signal, so we suppress the negative case. Verified in `TestRoundtripToolResultPart`.
3. **`PartUnknown` re-marshals to the same bytes** — When we hit an unknown discriminator we stash `data` into `Raw`, and `MarshalJSON` returns `Raw` verbatim for `PartUnknown`. This makes the encode/decode/encode cycle a fixed-point even on inputs we don't structurally understand — a gift for the s06 streaming layer when an unknown event slips through.

## What Changed (vs. s01)

```diff
 type Message struct {
-    Role    string         `json:"role"`
-    Content []ContentBlock `json:"content"`
+    ID      string `json:"id,omitempty"`
+    Role    string `json:"role"`
+    Content []Part `json:"content"`
 }

-// only carried text in s01
-type ContentBlock struct {
-    Type string `json:"type"`
-    Text string `json:"text,omitempty"`
-}
+// Tagged union: exactly one of the *Ref fields is non-nil per Kind.
+type Part struct {
+    Kind       PartKind
+    Text       *TextRef
+    ToolUse    *ToolUseRef
+    ToolResult *ToolResultRef
+    File       *FileRef
+    Reasoning  *ReasoningRef
+    Snapshot   *SnapshotRef
+    Patch      *PatchRef
+    Raw        json.RawMessage // PartUnknown payload
+}
```

Wire format is unchanged: every Part still serializes to `{"type":"...", ...}`. So an HTTP layer written against s01's `ContentBlock` can decode an s02 text Part without modification — only the consumer-side switch widens.

## Try It

```bash
cd agents/s02-message-parts

# Demo: build a 3-part message, marshal it, decode it back.
go run .

# 5 tests, no network.
go test -count=1 ./...

# Show the wire JSON of a single tool_use part:
go run . | sed -n '/wire JSON/,/decoded/p' | head -30
```

## Upstream Source Reading

The mechanism this s02 mirrors lives in opencode's `packages/opencode/src/session/message-v2.ts` lines 76–123. It defines `partBase` (the common id/sessionID/messageID fields) plus four of the seven Part variants we model in Go. opencode uses Effect's `Schema.Struct` + `Schema.Literal("text")` to express the same tagged union; the wire JSON is identical.

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

Permalink: <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-v2.ts#L76-L123>

What we kept and what we dropped:

- **Kept** — the `type:` discriminator, the per-variant struct, the JSON shape.
- **Dropped (for now)** — `partBase` (no Session table until s07), `time` and `metadata` (UI/compaction details), `synthetic` and `ignored` (later compaction signals), and the `ToolPart.state` machine (`pending|running|completed|error`) — we model that as a flat `ToolResultRef` and revisit in s10's tool execution loop.
- **Forward-compat** — opencode keeps adding new variants (`compaction`, `step-start`, `retry`, `agent`, `subtask`); our `PartUnknown` arm lets the loop survive each release without us having to ship a Go-side update first.

Reading order for opencode's message model:
1. `packages/opencode/src/session/message-v2.ts` lines 76–207 — every Part variant
2. `packages/opencode/src/session/message-v2.ts` lines 248–320 — `ToolState` + `ToolPart` (s10 territory)
3. `packages/opencode/src/session/processor.ts` lines 34–150 — what consumes the Part stream (s09 territory)
