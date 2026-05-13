# s07 — session-store

s02 gave us a `Message` of `Parts`. s06 gave us a `Loop` that *assembles*
streaming Events into one. s07 gives those Messages a *home*: a
SQLite-backed `sessions / messages / parts` schema that round-trips the
same Part union you've been building since s02. Every later session that
needs persistence (s10's tool loop, s14's cost roll-up, a future TUI)
opens this DB instead of holding state in RAM.

The store is deliberately narrow: three tables, four CRUD funcs, no
transactions exposed to the caller, no migration framework. We use
**`modernc.org/sqlite`** — pure Go, no CGo — so CI builds without a C
toolchain.

## Files

- `parts.go` — `Message` + `Part` + 5 variant payloads (Text / ToolUse /
  ToolResult / Reasoning / File), re-implemented from s02. The on-disk
  format: one row per Part with a `kind` discriminator column and a
  `payload` JSON column for the variant fields.
- `session.go` — the `Session` struct (9 fields, the subset of upstream's
  23-column SessionTable that s07 actually persists).
- `db.go` — the new code:
  - `func Open(path string) (*DB, error)` — opens SQLite, runs CREATE
    TABLE IF NOT EXISTS for sessions/messages/parts, enables the
    `foreign_keys` pragma (off by default in SQLite for historical
    reasons; without it FK violations are silently accepted).
  - `func (db *DB) CreateSession(s *Session) error`
  - `func (db *DB) AppendMessage(sessionID string, m *Message) error` —
    persists Message + all Parts in one transaction.
  - `func (db *DB) GetSession(id string) (*Session, []*Message, error)` —
    loads session + messages (created_at order) + Parts (position order).
  - `func (db *DB) ListSessions(limit int) ([]*Session, error)` —
    newest-first by updated_at.
- `main.go` — short demo. Opens `:memory:`, creates a session, appends
  user + assistant messages with mixed Parts (text + tool_use + text),
  reads the session back, prints as JSON. Deterministic, no network.
- `db_test.go` — 5 tests, all using `:memory:` DBs:
  1. **CreateAndGetSession** — round-trip a session with all 9 fields.
  2. **AppendMessageWithMixedParts** — text + tool_use + text + reasoning
     comes back in *position* order (the s06 boundary rule, re-asserted
     at the persistence layer).
  3. **AppendTwoMessagesOrdered** — messages load in `created_at` order,
     not insert order.
  4. **ListSessionsNewestFirst** — `updated_at DESC` ordering, plus
     `limit` honored.
  5. **ForeignKeyRejectsOrphanMessage** — `AppendMessage` to a
     non-existent session returns an FK error; `GetSession` of a
     missing session returns `sql.ErrNoRows`.

## Run

```
# Demo (deterministic, no network)
go run .

# 5 tests
go test -count=1 ./...

# Vet + build + test in one go
go vet ./... && go build ./... && go test -count=1 ./...
```

## Key teaching points

- **Storage is just another consumer of Parts.** s07 doesn't change the
  Part union — it persists the same shape s02 / s06 already produced.
  The discriminator (`kind` column) + JSON payload pattern is the SQL
  analogue of Go's tagged-union-via-pointer trick.
- **`payload` is JSON, but `kind` is its own column.** Indexing or
  filtering on Kind doesn't have to JSON-scan every row. Same trick the
  upstream `session.sql.ts` uses.
- **`created_at` / `position` are the load-bearing sort keys.** Messages
  by `created_at` ASC; Parts within a Message by `position` ASC. Insert
  order is *not* a substitute — tests #2 and #3 prove it.
- **Pure-Go SQLite (`modernc.org/sqlite`).** Registers as the `"sqlite"`
  driver. No CGo, builds in any CI without `apt install build-essential`.
  Mattn's CGo driver is faster but the build complexity isn't worth it
  for a teaching session.
- **`foreign_keys` pragma is OFF by default in SQLite.** We turn it on
  via the DSN (`?_pragma=foreign_keys(1)`). Without this, test #5 would
  silently pass and orphan rows would accumulate undetected.
- **One transaction per `AppendMessage`.** Either the Message + all its
  Parts land, or none do. The alternative — Message inserted but a Part
  fails — would produce a "loaded session has a Message with missing
  Parts" data-corruption shape that s10 would suffer for silently.

See `docs/zh/s07-session-store.md` and `docs/en/s07-session-store.md`
for the long-form walkthrough plus the upstream `session/session.ts`
excerpt that motivates the schema.
