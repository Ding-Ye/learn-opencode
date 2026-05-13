package main

import "time"

// Session is one persisted conversation thread. The fields mirror the
// minimum subset of opencode's SessionTable (packages/opencode/src/session/
// session.sql.ts) that s07 needs to make the round-trip useful — the
// upstream table has 23 columns; we keep 9, the ones that survive every
// later session in this curriculum:
//
//   - ID:           caller-assigned (typically a ULID).
//   - Slug:         human-readable handle.
//   - ProjectID:    the workspace this session belongs to (s09 will care).
//   - ParentID:     non-empty when this is a sub-session forked from
//                   another (the "child session" in upstream terms).
//   - CreatedAt /   timestamps; persisted as Unix ms in the DB so they
//     UpdatedAt:    sort lexicographically and survive timezone changes.
//   - Cost / Input/ token + cost accounting; s14 fills these in. s07 just
//     OutputTokens:  reserves the columns so s14 doesn't need a migration.
//
// The fields opencode also has but s07 deliberately drops:
//   - workspace_id, directory, path, title, version    — agent runtime data
//   - share_url, summary_*, revert, permission, agent  — feature-specific
//   - model JSON blob, time_compacting, time_archived  — handled later
//
// The point of the cut: keep the schema small enough that the test
// fixtures stay readable. Every later session that needs a column just
// ALTER-TABLEs it on; no migration framework yet.
type Session struct {
	ID           string
	Slug         string
	ProjectID    string
	ParentID     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Cost         float64
	InputTokens  int
	OutputTokens int
}
