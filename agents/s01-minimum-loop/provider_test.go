package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer returns an httptest.Server that asserts request shape and replies
// with a canned response.
func stubServer(t *testing.T, status int, body string, captured **CreateMessageRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong anthropic-version: %q", r.Header.Get("anthropic-version"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("wrong Content-Type: %q", r.Header.Get("Content-Type"))
		}
		buf, _ := io.ReadAll(r.Body)
		if captured != nil {
			var req CreateMessageRequest
			if err := json.Unmarshal(buf, &req); err == nil {
				*captured = &req
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestAnthropicProviderRequestShape(t *testing.T) {
	var got *CreateMessageRequest
	srv := stubServer(t, 200,
		`{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`,
		&got)
	defer srv.Close()

	p := NewAnthropicProvider("test-key", "claude-test").withBaseURL(srv.URL)
	_, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		System: "be brief",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("server didn't capture request")
	}
	if got.Model != "claude-test" {
		t.Errorf("Model not defaulted from provider: %q", got.Model)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("MaxTokens default = %d, want 4096", got.MaxTokens)
	}
	if got.System != "be brief" {
		t.Errorf("System lost: %q", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "hello" {
		t.Errorf("Messages mangled: %+v", got.Messages)
	}
}

func TestAnthropicProviderParseResponse(t *testing.T) {
	srv := stubServer(t, 200,
		`{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"world"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`,
		nil)
	defer srv.Close()

	p := NewAnthropicProvider("k", "m").withBaseURL(srv.URL)
	resp, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "world" {
		t.Errorf("Content mangled: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 2 {
		t.Errorf("Usage mangled: %+v", resp.Usage)
	}
}

func TestAnthropicProviderHTTPError(t *testing.T) {
	srv := stubServer(t, 401, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`, nil)
	defer srv.Close()

	p := NewAnthropicProvider("bad", "m").withBaseURL(srv.URL)
	_, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401, got %q", err.Error())
	}
}

func TestAnthropicProviderModelOverride(t *testing.T) {
	var got *CreateMessageRequest
	srv := stubServer(t, 200,
		`{"id":"msg","role":"assistant","content":[{"type":"text","text":""}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		&got)
	defer srv.Close()

	p := NewAnthropicProvider("k", "default-model").withBaseURL(srv.URL)
	_, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Model:    "explicit-model", // explicit override should win
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "."}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "explicit-model" {
		t.Errorf("explicit Model lost: %q", got.Model)
	}
}
