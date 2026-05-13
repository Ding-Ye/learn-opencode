package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

// main is a hand-runnable demo of the session store:
//
//	go run .
//
// Opens an in-memory SQLite DB, creates one session, appends two messages
// (the user "edit a.go" + the assistant "OK, calling edit + done."), then
// reads the session back via GetSession and prints it as JSON. Output is
// deterministic, no network, no API key, no on-disk file left behind.
//
// In s10 the demo will be replaced by one that runs a real streaming-loop
// turn against a live Provider and persists each assembled Message into
// the same DB. The CreateSession / AppendMessage call sites stay unchanged.
func main() {
	db, err := Open(":memory:")
	if err != nil {
		log.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	sess := &Session{
		ID:        "sess_demo_1",
		Slug:      "demo-rename-foo",
		ProjectID: "proj_local",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateSession(sess); err != nil {
		log.Fatalf("CreateSession: %v", err)
	}

	// Message 1: the user's request.
	userMsg := &Message{
		ID:        "msg_user_1",
		Role:      RoleUser,
		CreatedAt: now.UnixMilli(),
		Parts: []Part{
			{Kind: PartText, Text: &TextPart{Text: "edit a.go: rename Foo to Bar"}},
		},
	}
	if err := db.AppendMessage(sess.ID, userMsg); err != nil {
		log.Fatalf("AppendMessage user: %v", err)
	}

	// Message 2: the assistant's reply with a tool_use sandwiched between
	// two text Parts — the same shape s06's Loop assembles from a stream.
	asstMsg := &Message{
		ID:        "msg_asst_1",
		Role:      RoleAssistant,
		CreatedAt: now.Add(time.Millisecond).UnixMilli(),
		Parts: []Part{
			{Kind: PartText, Text: &TextPart{Text: "calling edit "}},
			{Kind: PartToolUse, ToolUse: &ToolUsePart{
				ID:    "toolu_demo_1",
				Name:  "edit",
				Input: json.RawMessage(`{"path":"a.go","old":"Foo","new":"Bar"}`),
			}},
			{Kind: PartText, Text: &TextPart{Text: "done."}},
		},
	}
	if err := db.AppendMessage(sess.ID, asstMsg); err != nil {
		log.Fatalf("AppendMessage assistant: %v", err)
	}

	// Read back the whole session — same shape we'd hand to s10's tool
	// loop or s14's cost roll-up.
	loadedSess, msgs, err := db.GetSession(sess.ID)
	if err != nil {
		log.Fatalf("GetSession: %v", err)
	}

	out := map[string]any{
		"session":  loadedSess,
		"messages": msgs,
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Fprintln(os.Stdout, string(pretty))
}
