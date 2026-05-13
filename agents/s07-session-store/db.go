package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	// Pure-Go SQLite driver. Registers itself as the "sqlite" driver, so
	// `sql.Open("sqlite", path)` works without CGo. We picked modernc.org/
	// sqlite over mattn/go-sqlite3 specifically because the latter needs a
	// C toolchain in CI; a pure-Go driver builds anywhere `go build` does.
	_ "modernc.org/sqlite"
)

// DB is the s07 storage handle. Wraps a *sql.DB so callers don't depend
// directly on database/sql plumbing — and so a future session (s14) can
// add transactions or batched writes without changing the surface.
//
// Concurrency note: *sql.DB is goroutine-safe. SQLite serializes writes
// under the hood; on a `:memory:` DB that's fine for our 5 tests, on a
// file-backed DB you'd want WAL mode + a single writer goroutine.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) a SQLite database at `path` and runs the
// idempotent CREATE TABLE statements for the three tables s07 owns:
// sessions, messages, parts. Foreign-key enforcement is enabled via the
// `?_pragma=foreign_keys(1)` query string — SQLite defaults to OFF for
// historical reasons, and without this the FK constraint test would
// silently pass instead of catching the misuse.
//
// Use ":memory:" for tests; the database is destroyed when DB.Close() is
// called. Use a filename for the demo / real use.
func Open(path string) (*DB, error) {
	dsn := path
	// Append the foreign_keys pragma — modernc.org/sqlite parses the
	// query string for `_pragma=` directives and runs them on connect.
	// We probe for an existing `?` so this composes with caller-passed
	// query strings without surprise.
	if path == ":memory:" {
		// In-memory DBs across multiple connections need a shared cache,
		// or each connection sees a fresh empty DB. The cache=shared form
		// matters once the connection pool grows past 1.
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys(1)"
	} else {
		sep := "?"
		for i := 0; i < len(path); i++ {
			if path[i] == '?' {
				sep = "&"
				break
			}
		}
		dsn = path + sep + "_pragma=foreign_keys(1)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// Single open connection for in-memory DBs — keeps all writes hitting
	// the same per-connection memory. Multiple conns would each see
	// their own DB without cache=shared, and even with it, this avoids
	// the test flakiness of cross-connection visibility races.
	if path == ":memory:" {
		sqlDB.SetMaxOpenConns(1)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying *sql.DB. Idempotent; safe to defer.
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// migrate runs the CREATE TABLE IF NOT EXISTS statements. Hand-rolled
// rather than using golang-migrate so the s07 module has zero deps
// beyond modernc.org/sqlite — keeping the surface readable for a
// teaching session. s14 may revisit when ALTER TABLE migrations land.
func (db *DB) migrate() error {
	stmts := []string{
		// sessions: the parent row. Columns mirror the subset of
		// upstream's SessionTable s07 actually persists. ProjectID is
		// a free-form text column, not an FK to a projects table —
		// we don't have one yet (that lands in s09's territory).
		`CREATE TABLE IF NOT EXISTS sessions (
			id            TEXT PRIMARY KEY,
			slug          TEXT,
			project_id    TEXT,
			parent_id     TEXT,
			created_at    INTEGER,
			updated_at    INTEGER,
			cost          REAL,
			input_tokens  INTEGER,
			output_tokens INTEGER
		)`,

		// messages: child of sessions. The FK enforces that you can't
		// AppendMessage to a non-existent session — caught by test #5.
		// `created_at` is the sort key for "load this session's
		// messages in arrival order."
		`CREATE TABLE IF NOT EXISTS messages (
			id         TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role       TEXT,
			created_at INTEGER,
			FOREIGN KEY(session_id) REFERENCES sessions(id)
		)`,

		// parts: child of messages. `position` preserves the order
		// Parts were appended in (the load-bearing s06 boundary
		// — adjacent text deltas collapsed to one Part, tool_use
		// breaks the run, etc). `payload` is JSON-of-variant; the
		// `kind` column is the discriminator so queries can filter
		// on Kind without JSON-scanning every row.
		`CREATE TABLE IF NOT EXISTS parts (
			id         TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			kind       TEXT,
			payload    TEXT,
			position   INTEGER,
			FOREIGN KEY(message_id) REFERENCES messages(id)
		)`,

		// Indexes — the lookups s07 actually does. No general-purpose
		// "index everything" instinct; only the ones a measured test
		// would notice. messages-by-session is the hot path for
		// GetSession; parts-by-message is the inner loop.
		`CREATE INDEX IF NOT EXISTS messages_session_idx ON messages(session_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS parts_message_idx ON parts(message_id, position)`,
	}
	for _, s := range stmts {
		if _, err := db.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w (stmt=%q)", err, firstLine(s))
		}
	}
	return nil
}

// firstLine returns the first non-empty line of s — used to make migrate
// error messages point at the right CREATE statement without dumping all
// of it. Cheap and readable.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// CreateSession inserts a new session row. Caller assigns the ID
// (typically a ULID). If CreatedAt is zero we stamp time.Now() — a
// common Go ergonomic so demo code doesn't have to set timestamps
// by hand. UpdatedAt mirrors CreatedAt at insert time.
//
// Returns an error on ID collision (the PRIMARY KEY constraint) so the
// caller can detect "I tried to create the same session twice."
func (db *DB) CreateSession(s *Session) error {
	if db == nil || db.sql == nil {
		return errors.New("db: nil DB")
	}
	if s == nil {
		return errors.New("CreateSession: nil session")
	}
	if s.ID == "" {
		return errors.New("CreateSession: empty ID")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}

	_, err := db.sql.Exec(
		`INSERT INTO sessions
		 (id, slug, project_id, parent_id, created_at, updated_at, cost, input_tokens, output_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Slug, s.ProjectID, s.ParentID,
		s.CreatedAt.UnixMilli(), s.UpdatedAt.UnixMilli(),
		s.Cost, s.InputTokens, s.OutputTokens,
	)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	return nil
}

// AppendMessage persists a Message and all its Parts under the given
// session. Wrapped in a transaction so a partial write (message inserted
// but a Part fails) leaves the DB clean — the alternative is "loaded
// session has a Message with missing Parts," which would surface as a
// silent data-corruption bug in s10.
//
// On success the FK ensures sessionID exists; on failure (FK violation,
// PK collision on a duplicate ID) the whole transaction rolls back.
func (db *DB) AppendMessage(sessionID string, m *Message) error {
	if db == nil || db.sql == nil {
		return errors.New("db: nil DB")
	}
	if m == nil {
		return errors.New("AppendMessage: nil message")
	}
	if m.ID == "" {
		return errors.New("AppendMessage: empty message ID")
	}
	if sessionID == "" {
		return errors.New("AppendMessage: empty sessionID")
	}
	if m.CreatedAt == 0 {
		// Stamp Unix-ms now; preserves the column type and gives later
		// loads a deterministic sort key. Tests that need exact
		// ordering pass explicit CreatedAt values to bypass this.
		m.CreatedAt = time.Now().UnixMilli()
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("AppendMessage: begin tx: %w", err)
	}
	// defer Rollback is a no-op after a successful Commit — idiomatic
	// Go pattern for "always clean up unless we explicitly commit."
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(
		`INSERT INTO messages (id, session_id, role, created_at) VALUES (?, ?, ?, ?)`,
		m.ID, sessionID, string(m.Role), m.CreatedAt,
	); err != nil {
		return fmt.Errorf("AppendMessage: insert message: %w", err)
	}

	for i, p := range m.Parts {
		partID := p.ID
		if partID == "" {
			// Synthesize a deterministic ID from the message ID +
			// position so test fixtures don't have to set Part IDs.
			// Real callers (s10) will use ULIDs.
			partID = m.ID + "-p" + strconv.Itoa(i)
		}
		payload, err := p.payloadJSON()
		if err != nil {
			return fmt.Errorf("AppendMessage: part[%d] payload: %w", i, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO parts (id, message_id, kind, payload, position) VALUES (?, ?, ?, ?, ?)`,
			partID, m.ID, string(p.Kind), string(payload), i,
		); err != nil {
			return fmt.Errorf("AppendMessage: insert part[%d]: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AppendMessage: commit: %w", err)
	}
	return nil
}

// GetSession loads a session by ID, plus all its messages (in created_at
// order) with their Parts (in position order). Returns sql.ErrNoRows-
// wrapped error if the session doesn't exist — caller distinguishes
// missing from broken via errors.Is(err, sql.ErrNoRows).
//
// The query shape: 2 round-trips total (session + messages) plus 1
// per-message Parts fetch. Could be folded into one query with a JOIN,
// but the inflate-into-struct logic gets harder to read; we accept the
// chattier shape because s07 isn't a performance session.
func (db *DB) GetSession(id string) (*Session, []*Message, error) {
	if db == nil || db.sql == nil {
		return nil, nil, errors.New("db: nil DB")
	}
	if id == "" {
		return nil, nil, errors.New("GetSession: empty ID")
	}

	row := db.sql.QueryRow(
		`SELECT id, slug, project_id, parent_id, created_at, updated_at, cost, input_tokens, output_tokens
		 FROM sessions WHERE id = ?`,
		id,
	)
	s := &Session{}
	var createdAt, updatedAt int64
	if err := row.Scan(
		&s.ID, &s.Slug, &s.ProjectID, &s.ParentID,
		&createdAt, &updatedAt,
		&s.Cost, &s.InputTokens, &s.OutputTokens,
	); err != nil {
		// errors.Is(err, sql.ErrNoRows) is what callers check for
		// "session not found" — wrapping with %w keeps that intact.
		return nil, nil, fmt.Errorf("GetSession %q: %w", id, err)
	}
	s.CreatedAt = time.UnixMilli(createdAt)
	s.UpdatedAt = time.UnixMilli(updatedAt)

	// Messages in created_at order, ties broken by id for determinism.
	rows, err := db.sql.Query(
		`SELECT id, role, created_at FROM messages
		 WHERE session_id = ? ORDER BY created_at ASC, id ASC`,
		id,
	)
	if err != nil {
		return s, nil, fmt.Errorf("GetSession messages: %w", err)
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var role string
		if err := rows.Scan(&m.ID, &role, &m.CreatedAt); err != nil {
			return s, nil, fmt.Errorf("GetSession scan message: %w", err)
		}
		m.Role = Role(role)
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return s, nil, fmt.Errorf("GetSession messages iter: %w", err)
	}

	// Now fetch Parts per message. The N+1 here is intentional —
	// see the comment above on chattier vs. JOIN-and-inflate.
	for _, m := range msgs {
		parts, err := db.loadParts(m.ID)
		if err != nil {
			return s, nil, err
		}
		m.Parts = parts
	}
	return s, msgs, nil
}

// loadParts returns Parts for messageID in position order. Internal
// helper for GetSession; not exported because Parts without their
// Message rarely make sense to a caller.
func (db *DB) loadParts(messageID string) ([]Part, error) {
	rows, err := db.sql.Query(
		`SELECT id, kind, payload FROM parts
		 WHERE message_id = ? ORDER BY position ASC`,
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("loadParts %q: %w", messageID, err)
	}
	defer rows.Close()

	var parts []Part
	for rows.Next() {
		var (
			id      string
			kindStr string
			payload []byte
		)
		if err := rows.Scan(&id, &kindStr, &payload); err != nil {
			return nil, fmt.Errorf("loadParts scan: %w", err)
		}
		p, err := partFromRow(id, PartKind(kindStr), payload)
		if err != nil {
			return nil, fmt.Errorf("loadParts hydrate %q: %w", id, err)
		}
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loadParts iter: %w", err)
	}
	return parts, nil
}

// ListSessions returns up to `limit` sessions, newest-first by
// updated_at. limit <= 0 returns all rows — callers that don't want
// to think about cardinality pass 0. The ORDER BY ties are broken by
// id for deterministic test output.
func (db *DB) ListSessions(limit int) ([]*Session, error) {
	if db == nil || db.sql == nil {
		return nil, errors.New("db: nil DB")
	}
	q := `SELECT id, slug, project_id, parent_id, created_at, updated_at, cost, input_tokens, output_tokens
	      FROM sessions ORDER BY updated_at DESC, id DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		s := &Session{}
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&s.ID, &s.Slug, &s.ProjectID, &s.ParentID,
			&createdAt, &updatedAt,
			&s.Cost, &s.InputTokens, &s.OutputTokens,
		); err != nil {
			return nil, fmt.Errorf("ListSessions scan: %w", err)
		}
		s.CreatedAt = time.UnixMilli(createdAt)
		s.UpdatedAt = time.UnixMilli(updatedAt)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSessions iter: %w", err)
	}
	return out, nil
}
