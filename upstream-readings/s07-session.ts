// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/session.ts (the SessionTable schema + Service init + row adapters, L1-L110)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L1-L110
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s07 cares about this file:
//   This is the persistence layer's *Go-side reference*. opencode's session.ts
//   does three things that s07 mirrors in Go:
//
//     1. Imports the drizzle table definitions from `./session.sql` and the
//        connection pool from `@/storage/db` — the equivalent of our Go
//        `database/sql` + hand-written `CREATE TABLE` SQL.
//
//     2. Defines `fromRow(row: SessionRow): Info` — a SQLite row → in-memory
//        struct adapter. We have the equivalent in our `GetSession` /
//        `loadParts` Scan() calls + `partFromRow` helper.
//
//     3. Defines `toRow(info: Info)` — the reverse direction, used by every
//        Update path. Our `CreateSession` binds parameters directly in the
//        Exec() call instead of materializing an intermediate row struct.
//
//   The schema itself lives in session.sql.ts (next door); session.ts is the
//   *adapter layer* between Drizzle's schema and the rest of opencode's
//   `Info` business object. We Go-port both sides of that bridge — the
//   schema (CREATE TABLE in db.go) and the adapter (Scan + Exec in db.go).
//
//   The big choice s07 makes that this file motivates: keep three separate
//   columns (`kind` discriminator + `payload` JSON + `position` ordering)
//   instead of trying to fit a Part into one row of typed columns. opencode
//   does the same — `data text({ mode: "json" })` in PartTable.
//
// What we rebuilt in Go (s07):
//   - SessionTable schema (subset)         → CREATE TABLE sessions in db.go
//   - MessageTable schema (subset)         → CREATE TABLE messages in db.go
//   - PartTable schema (subset)            → CREATE TABLE parts in db.go
//   - SessionTable's `Timestamps` mixin    → created_at / updated_at INTEGER columns
//   - fromRow / toRow adapters             → Scan(&...) / Exec(?, ?, ...) in db.go
//   - drizzle's text({mode:"json"})        → blob -> json.Unmarshal in partFromRow
//   - Service init / connection pool       → Open() + migrate() in db.go
//
// What we DID NOT rebuild yet (lives in later sessions or out of scope):
//   - `workspace_id` + project FK cascade        — needs Project schema (s09)
//   - `directory` / `path` / `title` / `version` — UI / runtime metadata
//   - `share_url` / `summary_*` / `revert`        — feature-specific (sharing, compaction)
//   - `permission` JSON column                    — s04 stores rules per-call; s14 may persist
//   - `agent` / `model` JSON                      — s09 + s14
//   - `time_compacting` / `time_archived`         — out of scope until compaction lands
//   - drizzle migration framework                 — we use CREATE TABLE IF NOT EXISTS
//   - drizzle's $type<T>() runtime tagging        — Go generics aren't applied here
//   - Effect-typed Service constructor            — Open() is plain; no effect system
//   - Bus event publishing on session updates     — out of scope
//
// ---- begin upstream excerpt: packages/opencode/src/session/session.ts L1-L110 ----

import { Slug } from "@opencode-ai/core/util/slug"
import path from "path"
import { BusEvent } from "@/bus/bus-event"
import { Bus } from "@/bus"
import { Decimal } from "decimal.js"
// ↑ Decimal: cost is tracked as a fixed-point decimal to avoid float drift
//   on token-price multiplications. Our s07 stores cost as REAL (float64);
//   acceptable for teaching, s14 may revisit if rounding bugs surface.
import { type ProviderMetadata, type LanguageModelUsage } from "ai"
import { InstallationVersion } from "@opencode-ai/core/installation/version"

import { Database } from "@/storage/db"
// ↑ The connection pool. Our Go side: *sql.DB returned by Open(), wrapped in
//   our DB struct so callers don't depend on database/sql plumbing directly.
import { NotFoundError } from "@/storage/storage"
import { eq } from "drizzle-orm"
import { and } from "drizzle-orm"
import { gte } from "drizzle-orm"
import { isNull } from "drizzle-orm"
import { desc } from "drizzle-orm"
import { like } from "drizzle-orm"
import { inArray } from "drizzle-orm"
import { lt } from "drizzle-orm"
import { or } from "drizzle-orm"
// ↑ drizzle's query operators. We hand-write SQL strings instead — fewer deps,
//   easier to read for a teaching session. Cost is no compile-time validation
//   of column names / types; gain is the schema fits on one screen.
import { SyncEvent } from "../sync"
import type { SQL } from "drizzle-orm"
import { PartTable, SessionTable } from "./session.sql"
// ↑ The schema lives in session.sql.ts (next door). Our Go side: the schema
//   is the CREATE TABLE strings inside migrate(); no separate file because
//   the schema fits in 30 lines of SQL.
import { ProjectTable } from "../project/project.sql"
import { Storage } from "@/storage/storage"
import * as Log from "@opencode-ai/core/util/log"
import { MessageV2 } from "./message-v2"
// ↑ The Part union definition lives in message-v2.ts. Our Go side: parts.go
//   re-implements it (each session is its own module, no cross-imports).
import type { InstanceContext } from "../project/instance"
import { InstanceState } from "@/effect/instance-state"
import { Snapshot } from "@/snapshot"
import { ProjectID } from "../project/schema"
import { WorkspaceID } from "../control-plane/schema"
import { SessionID, MessageID, PartID } from "./schema"
import { ModelID, ProviderID } from "@/provider/schema"

import type { Provider } from "@/provider/provider"
import { Permission } from "@/permission"
import { Global } from "@opencode-ai/core/global"
import { Effect, Layer, Option, Context, Schema, Types } from "effect"
// ↑ Effect: opencode's runtime is built on Effect-TS — every fn returns
//   Effect.Effect<A, E, R>. We translate to plain Go funcs that return
//   (T, error). Lossy at scale but right for a 14-chapter curriculum.
import { NonNegativeInt, optionalOmitUndefined } from "@opencode-ai/core/schema"
import { RuntimeFlags } from "@/effect/runtime-flags"

const log = Log.create({ service: "session" })

const parentTitlePrefix = "New session - "
const childTitlePrefix = "Child session - "

function createDefaultTitle(isChild = false) {
  return (isChild ? childTitlePrefix : parentTitlePrefix) + new Date().toISOString()
}
// ↑ Default title generator for the UI. s07 has no UI / Title field; cut.

export function isDefaultTitle(title: string) {
  return new RegExp(
    `^(${parentTitlePrefix}|${childTitlePrefix})\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}\\.\\d{3}Z$`,
  ).test(title)
}

type SessionRow = typeof SessionTable.$inferSelect
// ↑ drizzle's signature move: derive the row type from the schema. Go has
//   no equivalent inference — we list the column types explicitly inside
//   `Scan(&col1, &col2, ...)`. The cost is a missed-rename here; the gain
//   is the schema is plain SQL strings any developer can read.

export function fromRow(row: SessionRow): Info {
  // ↑ The schema → in-memory adapter. Our Go equivalent is the body of
  //   GetSession: it does the same thing (Scan into a Session struct,
  //   convert int64 ms columns into time.Time, etc.) — just inlined into
  //   the query path instead of pulled out as a named adapter, because
  //   our Session struct is flat enough that a separate adapter wouldn't
  //   carry its weight.
  const summary =
    row.summary_additions !== null || row.summary_deletions !== null || row.summary_files !== null
      ? {
          additions: row.summary_additions ?? 0,
          deletions: row.summary_deletions ?? 0,
          files: row.summary_files ?? 0,
          diffs: row.summary_diffs ?? undefined,
        }
      : undefined
  // ↑ Composite-from-nullable-columns pattern: 3 separate columns collapse
  //   into one optional `summary` object. Our Session has no summary; if it
  //   did, the Go pattern would be sql.NullInt64 + manual nil-check.
  const share = row.share_url ? { url: row.share_url } : undefined
  const revert = row.revert ?? undefined
  // ↑ `share` and `revert` are JSON columns; drizzle auto-parses on read.
  //   In Go we'd json.Unmarshal manually (see partFromRow for that pattern).
  return {
    id: row.id,
    slug: row.slug,
    projectID: row.project_id,
    workspaceID: row.workspace_id ?? undefined,
    directory: row.directory,
    path: row.path ?? undefined,
    parentID: row.parent_id ?? undefined,
    title: row.title,
    agent: row.agent ?? undefined,
    model: row.model
      ? {
          id: ModelID.make(row.model.id),
          providerID: ProviderID.make(row.model.providerID),
          variant: row.model.variant,
        }
      : undefined,
    // ↑ `model` is a JSON object column with three fields. drizzle gave us
    //   the parsed object; we re-tag the brands (ModelID.make / ProviderID.make).
    //   Go has no brand types in the type system; we'd just use plain strings.
    version: row.version,
    summary,
    cost: row.cost,
    tokens: {
      input: row.tokens_input,
      output: row.tokens_output,
      reasoning: row.tokens_reasoning,
      cache: {
        read: row.tokens_cache_read,
        write: row.tokens_cache_write,
      },
    },
    // ↑ Nested tokens shape — 5 columns flatten to a tree on the way in.
    //   Our s07 Session keeps just 2 token fields (input/output) flat at the
    //   top level. s14 will add `TokensReasoning`, `TokensCacheRead`,
    //   `TokensCacheWrite` as flat fields, no nesting; SQLite columns don't
    //   nest, so the in-memory shape may as well stay flat too.
    share,
    revert,
    permission: row.permission ?? undefined,
    time: {
      created: row.time_created,
      updated: row.time_updated,
      compacting: row.time_compacting ?? undefined,
      archived: row.time_archived ?? undefined,
    },
    // ↑ Nested `time` shape — 4 timestamp columns wrapped together. Our
    //   Session has CreatedAt + UpdatedAt as time.Time at the top level,
    //   coming from `time.UnixMilli(row.created_at_int64)`. Same data,
    //   different idiom.
  }
}

// ---- end upstream excerpt ----
//
// Reading map (in s07 order — later sessions read deeper):
//   1. session/session.sql.ts L16-L91 (SessionTable / MessageTable / PartTable)  — the schema (s07)
//   2. session/session.ts L1-L110 (imports + fromRow)                            — this excerpt (s07)
//   3. session/session.ts L112-L143 (toRow)                                      — the reverse direction (s07)
//   4. session/message-v2.ts (Part union)                                        — s02 covered basics; revisit through persistence lens (s07)
//   5. session/processor.ts L34-L150 (Event reducer + persistence)                — s10 will mirror this
//   6. storage/db.ts (connection pool + migrations)                               — corresponds to our Go Open() + migrate()
//
// The mental jump from upstream → s07 Go:
//   - drizzle ORM with $inferSelect / $type<T>()    → hand-written CREATE TABLE + Scan(&...)
//   - 23-column SessionTable                          → 9-column sessions table (subset)
//   - JSON column auto-parse on read                  → json.Unmarshal in partFromRow
//   - Effect-typed Service.run()                      → plain Go func returning (T, error)
//   - drizzle migration framework                     → CREATE TABLE IF NOT EXISTS
//   - per-table Timestamps mixin                      → created_at / updated_at INTEGER columns
//   - ProjectTable FK cascade                         → text column (no Project schema yet)
//   - drizzle query operator imports (eq/and/desc)    → SQL strings
//
// What stays identical: the *shape* of persistence — three tables linked by FK,
// `kind` as a discriminator column with `payload` JSON for variant data, sort
// keys (`created_at` for messages, `position` for parts) that survive insert
// order, and the round-trip invariant: what you AppendMessage'd is what
// GetSession returns. Neither side knows about the other's idioms; they only
// agree on the row shape and the constraint that load = inverse-of-store.
