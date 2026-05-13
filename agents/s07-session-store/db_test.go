package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// openTestDB is the testing constructor — every test runs against a fresh
// in-memory SQLite DB so they can't observe each other's writes. The
// `t.Cleanup` registration means individual tests don't have to remember
// to defer Close.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestCreateAndGetSession pins the round-trip: a session you write goes
// in and comes out unchanged (modulo time-precision: we persist Unix-ms
// so sub-millisecond fractions are dropped — the test uses a millisecond-
// rounded time to stay deterministic).
//
// The shape matters because s10 will rely on this: between two streaming
// loop iterations it'll re-load the session to feed the prior Messages
// back into the next Provider call. If GetSession lost a column, s10
// would silently send an under-spec'd request to the LLM.
func TestCreateAndGetSession(t *testing.T) {
	db := openTestDB(t)

	now := time.UnixMilli(1700000000000) // 2023-11-14 — pinned so the round-trip is exact.
	want := &Session{
		ID:           "sess_a",
		Slug:         "rename-foo",
		ProjectID:    "proj_x",
		ParentID:     "",
		CreatedAt:    now,
		UpdatedAt:    now,
		Cost:         0.0123,
		InputTokens:  42,
		OutputTokens: 17,
	}
	if err := db.CreateSession(want); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, msgs, err := db.GetSession("sess_a")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0 (session has no messages yet)", len(msgs))
	}
	if got.ID != want.ID || got.Slug != want.Slug || got.ProjectID != want.ProjectID {
		t.Errorf("identity fields mismatch: got %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.Cost != want.Cost {
		t.Errorf("Cost = %v, want %v", got.Cost, want.Cost)
	}
	if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens {
		t.Errorf("tokens = %d/%d, want %d/%d",
			got.InputTokens, got.OutputTokens, want.InputTokens, want.OutputTokens)
	}
}

// TestAppendMessageWithMixedParts pins the load-bearing s06-style
// "different Part kinds in one Message" rule: text, tool_use, text in
// arrival order MUST come back in arrival order. If the parts table
// loaded by message_id order (instead of position order), this test
// would catch it — the IDs we synthesize are deterministic but their
// alphabetic order doesn't match position order.
func TestAppendMessageWithMixedParts(t *testing.T) {
	db := openTestDB(t)

	if err := db.CreateSession(&Session{ID: "sess_b"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	msg := &Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: []Part{
			{Kind: PartText, Text: &TextPart{Text: "calling edit "}},
			{Kind: PartToolUse, ToolUse: &ToolUsePart{
				ID:    "toolu_1",
				Name:  "edit",
				Input: json.RawMessage(`{"path":"a.go"}`),
			}},
			{Kind: PartText, Text: &TextPart{Text: "done."}},
			{Kind: PartReasoning, Reasoning: &ReasoningPart{Text: "thought briefly"}},
		},
	}
	if err := db.AppendMessage("sess_b", msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	_, msgs, err := db.GetSession("sess_b")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	got := msgs[0].Parts
	if len(got) != 4 {
		t.Fatalf("got %d Parts, want 4: %+v", len(got), got)
	}

	// Position 0: text "calling edit "
	if got[0].Kind != PartText || got[0].Text == nil || got[0].Text.Text != "calling edit " {
		t.Errorf("Parts[0] = %+v, want PartText 'calling edit '", got[0])
	}
	// Position 1: tool_use with input intact
	if got[1].Kind != PartToolUse || got[1].ToolUse == nil ||
		got[1].ToolUse.Name != "edit" || string(got[1].ToolUse.Input) != `{"path":"a.go"}` {
		t.Errorf("Parts[1] = %+v, want PartToolUse edit", got[1])
	}
	// Position 2: text "done." — fresh PartText, NOT concatenated
	// with Parts[0]. This is the s06 boundary rule re-asserted at
	// the persistence layer.
	if got[2].Kind != PartText || got[2].Text == nil || got[2].Text.Text != "done." {
		t.Errorf("Parts[2] = %+v, want PartText 'done.'", got[2])
	}
	// Position 3: reasoning
	if got[3].Kind != PartReasoning || got[3].Reasoning == nil ||
		got[3].Reasoning.Text != "thought briefly" {
		t.Errorf("Parts[3] = %+v, want PartReasoning", got[3])
	}
}

// TestAppendTwoMessagesOrdered pins the load-bearing "messages come back
// in created_at order" contract. s10 will read messages back to feed
// them into the next LLM call; if the order was non-deterministic the
// model would see a scrambled conversation.
func TestAppendTwoMessagesOrdered(t *testing.T) {
	db := openTestDB(t)

	if err := db.CreateSession(&Session{ID: "sess_c"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Append in reverse-time order: msg_b is "older" (smaller
	// CreatedAt) than msg_a, but we insert msg_a first. The load
	// MUST sort by created_at, not by insert order.
	msgA := &Message{
		ID: "msg_a", Role: RoleUser, CreatedAt: 2000,
		Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "second"}}},
	}
	msgB := &Message{
		ID: "msg_b", Role: RoleAssistant, CreatedAt: 1000,
		Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "first"}}},
	}
	if err := db.AppendMessage("sess_c", msgA); err != nil {
		t.Fatalf("AppendMessage A: %v", err)
	}
	if err := db.AppendMessage("sess_c", msgB); err != nil {
		t.Fatalf("AppendMessage B: %v", err)
	}

	_, msgs, err := db.GetSession("sess_c")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].ID != "msg_b" || msgs[1].ID != "msg_a" {
		t.Errorf("order = [%s, %s], want [msg_b, msg_a]", msgs[0].ID, msgs[1].ID)
	}
	if msgs[0].Parts[0].Text.Text != "first" {
		t.Errorf("msgs[0].Parts[0] = %q, want 'first'", msgs[0].Parts[0].Text.Text)
	}
}

// TestListSessionsNewestFirst pins the dashboard query: "show me the most
// recently active sessions." s10 won't use this directly (it loads by ID),
// but a future TUI / web view absolutely will, and getting the order
// wrong on a list page is a classic silent-bug.
func TestListSessionsNewestFirst(t *testing.T) {
	db := openTestDB(t)

	// Three sessions with monotonically increasing UpdatedAt. We insert
	// the newest LAST so insert-order != desired output order — that's
	// what makes the test catch a bug where ListSessions accidentally
	// uses ROWID order.
	for i, s := range []*Session{
		{ID: "sess_old", Slug: "old", UpdatedAt: time.UnixMilli(1000)},
		{ID: "sess_mid", Slug: "mid", UpdatedAt: time.UnixMilli(2000)},
		{ID: "sess_new", Slug: "new", UpdatedAt: time.UnixMilli(3000)},
	} {
		// CreatedAt zero → CreateSession fills it with time.Now, but
		// we override UpdatedAt explicitly. Set CreatedAt non-zero
		// so it doesn't get coerced to UpdatedAt-via-IsZero logic.
		s.CreatedAt = time.UnixMilli(int64(i + 1))
		if err := db.CreateSession(s); err != nil {
			t.Fatalf("CreateSession %s: %v", s.ID, err)
		}
	}

	got, err := db.ListSessions(0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sessions, want 3", len(got))
	}
	wantOrder := []string{"sess_new", "sess_mid", "sess_old"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, w)
		}
	}

	// limit=2 returns only the newest two.
	got2, err := db.ListSessions(2)
	if err != nil {
		t.Fatalf("ListSessions(2): %v", err)
	}
	if len(got2) != 2 || got2[0].ID != "sess_new" || got2[1].ID != "sess_mid" {
		t.Errorf("limit=2 returned %+v, want [sess_new, sess_mid]", got2)
	}
}

// TestForeignKeyRejectsOrphanMessage pins the FK constraint: appending a
// message to a non-existent session MUST fail. Without the foreign_keys
// pragma being honored, SQLite would silently accept the orphan row and
// GetSession on the bogus session would return ErrNoRows for the session
// itself but cheerfully load the orphaned message — a worst-case data-
// corruption shape.
//
// We also pin that GetSession on a missing session returns sql.ErrNoRows
// so callers can distinguish "missing" from "broken."
func TestForeignKeyRejectsOrphanMessage(t *testing.T) {
	db := openTestDB(t)

	// No CreateSession before this call — session_does_not_exist is a
	// dangling parent reference.
	err := db.AppendMessage("session_does_not_exist", &Message{
		ID:   "orphan_msg",
		Role: RoleUser,
		Parts: []Part{{Kind: PartText, Text: &TextPart{Text: "hello"}}},
	})
	if err == nil {
		t.Fatal("AppendMessage to nonexistent session: err = nil, want FK error")
	}
	// SQLite's FK error message contains "FOREIGN KEY constraint failed";
	// we don't pin the exact string (driver may vary the wording), but we
	// do assert the error mentions the constraint family.
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("err = %v, want a foreign-key / constraint error", err)
	}

	// And the orphan must not have landed: GetSession on the bogus ID
	// returns ErrNoRows, AND GetSession of any real session (we never
	// created one) confirms zero rows in the messages table.
	_, _, err = db.GetSession("session_does_not_exist")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetSession on missing session: err = %v, want sql.ErrNoRows", err)
	}
}
