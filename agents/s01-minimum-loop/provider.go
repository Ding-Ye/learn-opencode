package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message is a single turn in the conversation. Anthropic's API takes a list
// of these (alternating user / assistant) plus an optional system prompt.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock mirrors opencode's Part union (TextPart | ToolUsePart | ...);
// at s01 we only carry text, but the JSON shape is already the wire form so
// later sessions can extend it without renaming.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type CreateMessageRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
}

type CreateMessageResponse struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Provider abstracts the LLM call so later sessions can swap Anthropic for
// OpenAI / Bedrock / a fake without touching the loop. This is opencode's
// `Provider` (packages/opencode/src/provider/provider.ts) reduced to the
// smallest interface that still admits a real implementation.
type Provider interface {
	CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error)
}

type AnthropicProvider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com/v1/messages",
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// withBaseURL is a test helper — production callers use NewAnthropicProvider.
func (a *AnthropicProvider) withBaseURL(url string) *AnthropicProvider {
	a.baseURL = url
	return a
}

func (a *AnthropicProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	if req.Model == "" {
		req.Model = a.model
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("anthropic API %d: %s", resp.StatusCode, string(respBody))
	}

	var out CreateMessageResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%s)", err, string(respBody))
	}
	return &out, nil
}
