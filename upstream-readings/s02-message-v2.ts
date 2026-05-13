// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/message-v2.ts (excerpt — partBase + 4 part schemas)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/message-v2.ts
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s02 cares about this file:
//   This is opencode's wire model for every message Part. The `Schema.Literal("text")`
//   discriminator + named struct per variant is exactly the tagged union our Go
//   `Part` / `PartKind` reproduces. The JSON shapes opencode emits — flat objects
//   with `type:` as the first field — match Anthropic's API verbatim, and so do ours.
//
// What we kept in Go:
//   - The `type` discriminator (our `PartKind` const)
//   - The 7 Part variants opencode actively dispatches on
//     (text, tool_use → ToolUse, tool_result → ToolResult, file, reasoning, snapshot, patch)
//   - One struct per variant, lifted into a single Go union via custom JSON
//
// What we DID NOT rebuild yet:
//   - `partBase` (sessionID, messageID, id) — those need a Session table; landing in s07
//   - `time`, `metadata`, `synthetic` optional fields — opencode uses them for UI
//     timestamping and compaction; not on the agent loop's critical path
//   - `ToolPart` with `state: ToolStatePending|Running|Completed|Error` — opencode's
//     in-flight tool tracking; we model it as a flat ToolResult Part for now (s10 expands)
//   - `FilePartSource` (FileSource | SymbolSource | ResourceSource) — extra source
//     metadata for citations; s11 (skills) and s13 (LSP) revisit
//   - `AgentPart`, `CompactionPart`, `SubtaskPart`, `RetryPart`, `StepStartPart`,
//     `StepFinishPart` — all later sessions
//
// ---- begin upstream excerpt (lines 76–123 of message-v2.ts) ----

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
export type SnapshotPart = Types.DeepMutable<Schema.Schema.Type<typeof SnapshotPart>>

export const PatchPart = Schema.Struct({
  ...partBase,
  type: Schema.Literal("patch"),
  hash: Schema.String,
  files: Schema.Array(Schema.String),
}).annotate({ identifier: "PatchPart" })
export type PatchPart = Types.DeepMutable<Schema.Schema.Type<typeof PatchPart>>

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
export type TextPart = Types.DeepMutable<Schema.Schema.Type<typeof TextPart>>

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
export type ReasoningPart = Types.DeepMutable<Schema.Schema.Type<typeof ReasoningPart>>
