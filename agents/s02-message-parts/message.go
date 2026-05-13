package main

// Message is one turn in the conversation. Compared to s01's Message — which
// carried `[]ContentBlock` of only text — s02's Content is `[]Part`, the
// tagged union from parts.go. The JSON wire format stays compatible because
// both versions emit `{"type": "...", ...}` objects under `content`.
//
// opencode's analogue lives in packages/opencode/src/session/message-v2.ts:
// MessageV2.Info has `id`, `role`, plus a separate `parts` table joined by
// messageID. We collapse the join here — Parts live inline, because s02 has
// no SQLite (s07 introduces persistence).
type Message struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role"`
	Content []Part `json:"content"`
}

// Roles mirror Anthropic's API. opencode adds an internal "tool" role for
// synthetic tool-result messages, but in the Anthropic wire format those
// land as user messages whose content is a single ToolResult Part — same
// shape we already support, so no enum required here.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)
