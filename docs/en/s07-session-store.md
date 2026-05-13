---
title: "s07 · Session storage"
chapter: 7
slug: s07-session-store
est_read_min: 12
---

# s07 · Session storage

> What this teaches: *persist* the same `Message` of `Parts` you've been building since s02. Three SQLite tables (sessions / messages / parts) + 4 CRUD funcs + adapter logic that turns Go structs into rows and back. Every later session that needs state across runs (s10, s14) opens this DB instead of holding it in RAM. SQLite via **`modernc.org/sqlite`** — pure Go, no CGo, so CI doesn't need a C toolchain.

---

## Problem

Through s06, the agent's `Message` lived entirely in memory. One `Loop.Consume` call returned a `*Message`, the function returned, the Message was gone. To build anything actually useful, we have to address:

- **Cross-process recovery.** User closes the CLI, comes back tomorrow — yesterday's conversation context has to still be there.
- **Multi-turn tool-loop intermediate state.** s10's tool loop runs N LLM round-trips per session, each producing a new Message; if the middle round dies (rate limit, Ctrl-C), the next launch must at least show "where we got to."
- **Cross-session references.** s09's sub-agent / parent-session relation (one session forks a child) needs the `parent_id` column to mean anything.
- **Token / cost accumulation.** s14 will roll up each Provider call's Usage into the session row — no row, nowhere to roll up.

Where to store it? opencode picks SQLite — single file, embedded, zero ops. We keep that, but we deliberately do *not* reach for an ORM (drizzle's job in TS); we hand-write SQL. The schema stays small, cut from 23 columns to 9, with the cost/token columns reserved so s14 doesn't need an ALTER TABLE.

Second constraint: **no CGo**. `mattn/go-sqlite3` is fast but its CGo build needs a C toolchain in CI. `modernc.org/sqlite` is a pure-Go transpiled SQLite, registers as the `"sqlite"` driver, and `sql.Open("sqlite", ...)` just works. Speed difference is irrelevant for teaching.

## Solution

Three tables + four functions, ~500 LOC total:

```go
type DB struct { /* wraps *sql.DB */ }

func Open(path string) (*DB, error)               // CREATE TABLE IF NOT EXISTS x3
func (db *DB) CreateSession(s *Session) error     // INSERT INTO sessions
func (db *DB) AppendMessage(sessionID string, m *Message) error  // tx: 1 message + N parts
func (db *DB) GetSession(id string) (*Session, []*Message, error)
func (db *DB) ListSessions(limit int) ([]*Session, error)
```

Schema:

| table | PK | key columns | role |
|---|---|---|---|
| `sessions` | `id` | `slug`, `project_id`, `parent_id`, `created_at`, `updated_at`, `cost`, `input_tokens`, `output_tokens` | one conversation thread |
| `messages` | `id` | `session_id` (FK), `role`, `created_at` | one message; FK to sessions |
| `parts` | `id` | `message_id` (FK), `kind`, `payload`, `position` | one Part; `payload` is JSON of variant fields, `kind` is the discriminator column |

Plus indexes `messages(session_id, created_at)` and `parts(message_id, position)` — the hot path for `GetSession`.

**Key `Open` detail**: the DSN includes `?_pragma=foreign_keys(1)`. SQLite has FK enforcement OFF by default for historical reasons; without this, test #5 (insert a message into a non-existent session) would silently succeed and orphan rows would pile up undetected.

**`AppendMessage` is wrapped in a transaction.** The Message row + N Part rows either all land or none do. A partial write — Message inserted, a Part fails — would leave a "Message without Parts" data-corruption shape that s10 would suffer for silently when it loads the session and feeds an empty-looking assistant turn back to the LLM.

**`GetSession` is N+1**: 1 query for session + 1 for messages + N for Parts. Could be folded into one JOIN with inflate-into-struct logic, but inflate gets harder to read. s07 isn't a performance session; we explicitly choose chatty-but-readable.

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s07 SQLite schema                                                      │
│                                                                        │
│   sessions ─┬─< messages ─┬─< parts                                     │
│             │             │                                            │
│   id (PK)   │   id (PK)   │   id (PK)                                  │
│   slug      │   session_id│   message_id (FK → messages.id)             │
│   project_id│   (FK → s.id)│  kind         ← discriminator column       │
│   parent_id │   role      │   payload      ← JSON of variant fields     │
│   created_at│   created_at│   position     ← sort key within message    │
│   updated_at│             │                                            │
│   cost      │             │                                            │
│   tokens_*  │             │                                            │
│                                                                        │
│   AppendMessage(sessionID, m):                                          │
│     tx.Begin()                                                          │
│     INSERT INTO messages (m.ID, sessionID, m.Role, m.CreatedAt)         │
│     for i, p := range m.Parts:                                          │
│       INSERT INTO parts (p.ID, m.ID, p.Kind, p.payloadJSON(), i)        │
│     tx.Commit()                                                         │
│                                                                        │
│   GetSession(id):                                                       │
│     SELECT * FROM sessions WHERE id = ?               → 1 row           │
│     SELECT * FROM messages WHERE session_id = ?                         │
│       ORDER BY created_at ASC, id ASC                 → N rows          │
│     for each msg:                                                       │
│       SELECT * FROM parts WHERE message_id = ?                          │
│         ORDER BY position ASC                          → M rows         │
└────────────────────────────────────────────────────────────────────────┘
```

**Four load-bearing design points**:

1. **`kind` is its own column; `payload` is JSON.** Queries like "find all tool_use Parts in session X" don't have to JSON-scan every row. Same idea the upstream `session.sql.ts` uses (`text({ mode: "json" })` for the payload + a separate type-discriminator at the message-v2 layer).
2. **`created_at` / `position` are *load-bearing* sort keys.** Messages sort by `created_at ASC`, Parts within a Message by `position ASC`. Insert order is *not* a substitute — tests #2 and #3 explicitly insert in reverse order to pin this. s10's next LLM call will feed history Messages back; wrong order means the model sees a scrambled conversation.
3. **The `foreign_keys` pragma must be ON.** SQLite legacy: defaults to OFF. We set `?_pragma=foreign_keys(1)` in the DSN so inserting a message into a non-existent session throws (test #5 pins this). Without it, orphan rows accumulate silently.
4. **`AppendMessage` is one transaction.** Message + all Parts commit together; any failure rolls back. Keeps the `GetSession` invariant "if I read a Message, I'll read all its Parts" always true.

**Why ~500 LOC**: because it does only four things — Open / CreateSession / AppendMessage / GetSession. No migration framework (`CREATE TABLE IF NOT EXISTS` is enough), no query builder (hand-written SQL), no connection-pool tuning beyond `SetMaxOpenConns(1)` for `:memory:`, no prepared-statement cache. Each new mechanism is just the next `database/sql` line.

## What Changed (vs. s02)

s07 is s02 with persistence — *not* layered on top of s06 (s06 added a streaming consumer; that's orthogonal to where the result is stored). So the diff is against s02:

```diff
 // s02: Part as an in-memory union, gone after the function returns.
 msg := &Message{
     ID:   "msg_1",
     Role: RoleAssistant,
     Content: []Part{
         {Kind: PartText,    Text:    &TextRef{Text: "hello"}},
         {Kind: PartToolUse, ToolUse: &ToolUseRef{Name: "edit"}},
     },
 }
-// Function returns, msg goes out of scope, gone.
-fmt.Println(msg.Content[0].Text.Text)

+// s07: same Part union, now with the ability to write it to SQLite and read it back.
+db, _ := Open("sessions.db")
+defer db.Close()
+_  = db.CreateSession(&Session{ID: "sess_1", Slug: "demo"})
+_  = db.AppendMessage("sess_1", msg)
+
+// Tomorrow, new process, RAM cleared — Message is still there.
+_, msgs, _ := db.GetSession("sess_1")
+fmt.Println(msgs[0].Parts[0].Text.Text) // "hello"
```

The Part shape doesn't change a line — that's proof s02's "Part is a tagged union" decision was right. s07 adds a *storage* layer, doesn't touch the *shape*.

Abstraction-boundary diff: before s07, `Message` was ephemeral; with SQLite layered on, `Message` is persistable — but the Go-side API is nearly unchanged (you still construct `Message{Parts: []Part{...}}`, plus one extra `db.AppendMessage(sessID, m)` call). That's the mark of a good storage abstraction: persistence concerns stay confined to explicit CRUD calls, no contamination of the in-memory types themselves.

A small detail to flag: s07's Part has fewer Kinds than s02's — no `Snapshot` / `Patch`. They get added when s10 / s14 land. Unrecognized Kinds in the `payload` column are tolerated (`partFromRow` doesn't error, it just leaves variant pointers nil) for forward-compat.

## Try It

```bash
cd agents/s07-session-store

# Demo (deterministic, no network, :memory: DB):
go run .

# 5 tests:
go test -count=1 ./...

# Vet + build + test in one go:
go vet ./... && go build ./... && go test -count=1 ./...
```

The 5 test scenarios:

1. **CreateAndGetSession** — all 9 fields round-trip; time precision down to milliseconds.
2. **AppendMessageWithMixedParts** — text + tool_use + text + reasoning come back in *position* order (not the ID's alphabetic order). The s06 boundary rule, re-asserted at the persistence layer.
3. **AppendTwoMessagesOrdered** — inserted reverse-time, loaded in `created_at` order.
4. **ListSessionsNewestFirst** — `updated_at DESC`; `limit` honored.
5. **ForeignKeyRejectsOrphanMessage** — `AppendMessage` to a missing session throws an FK error; `GetSession` of a missing session returns `sql.ErrNoRows`.

## Upstream Source Reading

s07 mirrors opencode's `packages/opencode/src/session/session.ts` (with the schema next door in `session.sql.ts`). The whole file is 1900 lines; s07 cares about L1-L110 — the imports lay out the session module's *dependency posture*, then `fromRow` translates a SQLite row into the in-memory `Info` struct, the schema → struct adapter. We use the same pattern in Go.

```ts
// upstream:packages/opencode/src/session/session.ts L1-L110

import { Slug } from "@opencode-ai/core/util/slug"
import path from "path"
import { BusEvent } from "@/bus/bus-event"
import { Bus } from "@/bus"
import { Decimal } from "decimal.js"
import { type ProviderMetadata, type LanguageModelUsage } from "ai"
import { InstallationVersion } from "@opencode-ai/core/installation/version"

import { Database } from "@/storage/db"
import { NotFoundError } from "@/storage/storage"
import { eq } from "drizzle-orm"
// ...other drizzle operator imports...
import { PartTable, SessionTable } from "./session.sql"
import { ProjectTable } from "../project/project.sql"
import { Storage } from "@/storage/storage"
import * as Log from "@opencode-ai/core/util/log"
import { MessageV2 } from "./message-v2"
// ...

const log = Log.create({ service: "session" })

const parentTitlePrefix = "New session - "
const childTitlePrefix = "Child session - "

function createDefaultTitle(isChild = false) {
  return (isChild ? childTitlePrefix : parentTitlePrefix) + new Date().toISOString()
}

type SessionRow = typeof SessionTable.$inferSelect

export function fromRow(row: SessionRow): Info {
  const summary =
    row.summary_additions !== null || row.summary_deletions !== null || row.summary_files !== null
      ? {
          additions: row.summary_additions ?? 0,
          deletions: row.summary_deletions ?? 0,
          files: row.summary_files ?? 0,
          diffs: row.summary_diffs ?? undefined,
        }
      : undefined
  const share = row.share_url ? { url: row.share_url } : undefined
  const revert = row.revert ?? undefined
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
    version: row.version,
    summary,
    cost: row.cost,
    tokens: {
      input: row.tokens_input,
      output: row.tokens_output,
      reasoning: row.tokens_reasoning,
      cache: { read: row.tokens_cache_read, write: row.tokens_cache_write },
    },
    share, revert,
    permission: row.permission ?? undefined,
    time: {
      created: row.time_created,
      updated: row.time_updated,
      compacting: row.time_compacting ?? undefined,
      archived: row.time_archived ?? undefined,
    },
  }
}
```

Line-by-line annotation (the load-bearing rows):

- **L9-L24 imports** — the two key lines: `Database from @/storage/db` (the connection pool) + `PartTable, SessionTable from ./session.sql` (the drizzle schema definitions). Our Go side maps to `database/sql` + hand-written `CREATE TABLE` SQL. drizzle gives type-safe query builder + automatic migrations; we have neither, but the schema is small enough not to need them.
- **L42 logger** — opencode uses a structured (service-tagged) logger. Our demo uses plain `fmt` / `log`; production code would obviously swap in `slog`.
- **L44-L49 default title** — "New session - 2026-05-13T10:30:00.000Z". Our `Session` has no Title field; that's UI copy and s07 has no UI.
- **L57 `type SessionRow = typeof SessionTable.$inferSelect`** — drizzle's signature move: derives the row type from the schema. Go doesn't need this kind of inference because the schema *is* a SQL string and the row's fields are listed manually in `Scan(&...)`.
- **L59-L110 `fromRow` adapter** — translates one SQLite row into an in-memory `Info` struct. Three details worth noting:
  - **L60-L68 nullable composite-field collapse** — if any of `summary_additions/deletions/files` is non-null, build a composite `summary` object. Go would use `sql.NullInt64` + manual checks, or sidestep the problem like our s07 does (no composite, fields listed flat — problem doesn't exist).
  - **L82-L88 JSON deserialization** — `row.model` is stored as `text({ mode: "json" })` in SQLite; drizzle JSON.parses automatically. Our Go side calls `json.Unmarshal` by hand (see `partFromRow`). Same effect, more explicit.
  - **L91-L99 nested tokens field** — upstream's tokens shape is `{input, output, reasoning, cache: {read, write}}` nested. Our s07 `Session` flattens to `InputTokens` + `OutputTokens` (cache columns added in s14), because nested structures don't map cleanly to flat SQLite columns.
- **L112-L143 `toRow` adapter** — the reverse direction, struct → row. Each `?? null` / `?? 0` is in-memory `undefined` → SQLite `NULL` / default-value translation. Our `CreateSession` is one SQL line that binds parameters directly — equivalent but flatter.

Permalinks:

- session.ts schema + adapters (L1-L110): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L1-L110>
- session.sql.ts (drizzle table definitions): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.sql.ts#L16-L91>
- Full toRow (L112-L143): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L112-L143>

What we kept, what we cut:

- **Kept** — the three-table structure (session / message / part), the `kind` discriminator + `payload` JSON union pattern, `created_at`-ordered loads, FK enforcement, the whole-transaction `AppendMessage`, the `fromRow` / `toRow` mental model (in Go that's `Scan` / `Exec` binding fields directly).
- **Cut for now** — `workspace_id` / `directory` / `path` / `title` / `version` / `share_url` / `summary_*` / `revert` / `permission` JSON / `agent` / `model` JSON / `time_compacting` / `time_archived` — about 14 columns. Each gets ALTER-TABLEd in by the session that needs it (s09 agent registry, s10 tool loop, s14 cost/recovery, future share / compaction).
- **Forward-compat** — the `Session` struct already has `Cost / InputTokens / OutputTokens`; s14 adds `tokens_reasoning / tokens_cache_*` by adding columns + extending the SELECT/INSERT field lists. s10 uses `AppendMessage` to land each tool-loop turn — signature unchanged. s_full's end-to-end wiring of s06 → s07 → s10 reads back via `GetSession` → `[]*Message`, no intermediate translation needed.

Reading order for opencode's session layer:

1. `packages/opencode/src/session/session.sql.ts` L16-L91 — the drizzle schema definitions (the *table* part of what s07 mirrors)
2. `packages/opencode/src/session/session.ts` L1-L143 — Service init + fromRow / toRow (the *adapter* part of what s07 mirrors, this excerpt)
3. `packages/opencode/src/session/message-v2.ts` — full Part union definition (s02 covered the basics; s07 re-reads through the persistence lens)
4. `packages/opencode/src/session/processor.ts` L34-L150 — reduces streaming Events into a Message and persists (s10 will add this on our side)
5. `packages/opencode/src/storage/db.ts` — connection pool / migrations (corresponds to our Go `Open` + `migrate`)
