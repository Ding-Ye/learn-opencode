# s02 — message-parts

s01 carried a single `[]ContentBlock` of text. A real coding agent doesn't return a single blob — it interleaves text with tool calls, tool results, files, reasoning, and (later) snapshots. s02 generalizes that into a tagged-union `Part` type, mirroring opencode's `packages/opencode/src/session/message-v2.ts`.

The wire format (JSON) is unchanged from s01: every Part still serializes as `{"type": "...", ...}`. So nothing downstream of the loop has to know about Parts vs ContentBlocks — they look identical to Anthropic.

## Files

- `parts.go` — `Part` struct + `PartKind` enum + 7 variant payloads (`TextRef`, `ToolUseRef`, `ToolResultRef`, `FileRef`, `ReasoningRef`, `SnapshotRef`, `PatchRef`); custom `MarshalJSON` / `UnmarshalJSON` that map between Go's "one struct + active pointer" shape and the flat `{type:..., ...}` wire shape.
- `message.go` — `Message{ID, Role, Content []Part}`. Same JSON envelope as s01's Message, but Content is now Parts.
- `main.go` — runnable demo: builds a 3-part assistant message (text + tool_use + reasoning), marshals to JSON, prints the wire form, then unmarshals back and prints typed.
- `parts_test.go` — 5 tests:
  1. text-part roundtrip
  2. tool_use-part roundtrip (with `name` + `input` map)
  3. tool_result-part roundtrip (with `tool_use_id` + `content` + `is_error`); also confirms `is_error: false` is omitted
  4. mixed message (text + tool_use) roundtrip — order preserved
  5. unknown part kind decodes as `PartUnknown` without panic, and re-marshals to identical bytes

## Run

```
go run .                # demo: marshal a mixed message, decode it back
go test -count=1 ./...  # 5 tests, no network
```

## What this maps to upstream

| This file              | Upstream file                                                      |
|------------------------|--------------------------------------------------------------------|
| `parts.go` Part union  | `packages/opencode/src/session/message-v2.ts` (the part schemas)   |
| `message.go` Message   | `packages/opencode/src/session/message-v2.ts` `MessageV2.Info`     |

## Key teaching points

- **Tagged unions in Go** = one struct + a `Kind` discriminator + N optional pointer fields, plus custom JSON. Not as elegant as Rust enums or TS `Schema.Union`, but JSON-symmetric and obvious to read.
- **Forward-compat decode** — unknown `type` values become `PartUnknown` with original bytes preserved in `Raw`. opencode's wire format gains new part kinds (`compaction`, `step-start`, `retry`...) every minor release; refusing to decode anything unknown would be a footgun.
- **Discriminator-flat wire** — the JSON is `{"type": "...", ...payload}`, not `{"type":"...", "data":{...}}`. This matches Anthropic's API verbatim, so no envelope/peel layer in any later session.
- **`is_error` is `omitempty`** — a tool result without an error must not emit `"is_error": false`, because the LLM should not be biased into thinking we're flagging an error state. Verified in `TestRoundtripToolResultPart`.
- **Order matters** — `[]Part` is a sequence. opencode's `processor.ts` reduces over Parts in order; if we ever switched to a `map[Kind]Part` we'd silently lose the temporal "text → tool_use → result" flow.

## What changed vs s01

s01's `ContentBlock{Type, Text}` only carried text. s02 replaces it with `Part`, a discriminated union over 7 kinds. Existing s01-shape JSON (text-only) still decodes fine because `{"type":"text","text":"..."}` → `Part{Kind:PartText, Text:&TextRef{...}}`.

See `docs/zh/s02-message-parts.md` and `docs/en/s02-message-parts.md` for the long-form walkthrough plus the upstream excerpt.
