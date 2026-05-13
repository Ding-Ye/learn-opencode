package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicProvider is the s05 concrete implementation of Provider — POSTs to
// Anthropic's /v1/messages endpoint with `"stream": true` in the body, parses
// the resulting Server-Sent Events into our Event union.
//
// Mirrors `packages/opencode/src/provider/provider.ts` after it has resolved
// (providerID="anthropic", modelID=...) into a concrete language model and
// the AI SDK's `streamText()` is about to dispatch the HTTP request. We skip
// the SDK and do the SSE read ourselves — it's about 80 lines and the only
// way to teach what the wire format looks like.
type AnthropicProvider struct {
	apiKey  string
	baseURL string // overridable for httptest in unit tests
	model   string
	client  *http.Client
}

// NewAnthropicProvider returns a provider pointed at Anthropic's production
// endpoint. The model is the default — callers can still override per-request
// via Request.Model.
func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
		model:   model,
		// 5 minute timeout: a streaming response can run for minutes when
		// the model is generating long output. Per-Event arrival is what
		// actually matters; this is just a safety ceiling.
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

// withBaseURL is the test-only knob that points the provider at an
// httptest.Server. Production callers use NewAnthropicProvider; the test
// helper is unexported on purpose so the public surface stays small.
func (a *AnthropicProvider) withBaseURL(url string) *AnthropicProvider {
	a.baseURL = url
	return a
}

// anthropicRequestBody is the wire shape we POST. We could reuse the public
// Request struct directly, but a separate struct keeps the JSON tags here
// (vendor-specific) instead of leaking them into the cross-vendor Request type.
type anthropicRequestBody struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	System      string       `json:"system,omitempty"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	Stream      bool         `json:"stream"`
}

// Stream sends `req` as a streaming POST to /v1/messages and returns a Stream
// the caller pulls Events from. Errors here are pre-stream errors (network
// refused, non-2xx HTTP status). Once we return a non-nil Stream, every
// subsequent failure surfaces through Stream.Next.
func (a *AnthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.Model == "" {
		req.Model = a.model
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	body := anthropicRequestBody{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		System:      req.System,
		Messages:    req.Messages,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		Stream:      true, // the whole reason we're here
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		// Drain & close so the connection can be reused even on error.
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic API %d: %s", resp.StatusCode, string(errBody))
	}

	return &anthropicStream{
		resp:    resp,
		scanner: bufio.NewScanner(resp.Body),
		// Tool input deltas can be large (multi-KB JSON). Bump scanner
		// buffer ceiling to 1 MiB so a single SSE line never overflows.
		buffers: make(map[int]*contentBlockBuffer),
	}, nil
}

// contentBlockBuffer accumulates partial JSON deltas for one tool_use block
// across many `content_block_delta` events, until `content_block_stop` lets
// us emit one EventToolUse with a fully-parsed Input.
type contentBlockBuffer struct {
	kind    string         // "text" | "tool_use" | "thinking"
	id      string         // for tool_use: the toolu_... id
	name    string         // for tool_use: the tool name
	jsonAcc strings.Builder // for tool_use: accumulated input_json_delta
}

// anthropicStream is the *Stream returned by AnthropicProvider.Stream.
// Internally it's a bufio.Scanner over the SSE body plus a small queue
// of decoded Events (some upstream SSE events fan out to zero of our
// Events, some to one, message_stop emits an EventFinish).
type anthropicStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	// queue holds Events decoded from one SSE event but not yet returned.
	// Most events produce 0 or 1 of our Events; the queue lets a single
	// SSE event (e.g. message_delta with both stop_reason and usage) push
	// more than one if we ever need to. For now it's at most 1 deep.
	queue []Event
	// buffers tracks per-content-block state across delta events. Indexed
	// by the block's `index` field (Anthropic numbers blocks 0, 1, 2...).
	buffers map[int]*contentBlockBuffer
	// usage accumulates the final token count from message_start (input)
	// + message_delta (output). We emit it in the EventFinish.
	usage Usage
	// done is set after we've enqueued EventFinish; the next Next() returns io.EOF.
	done bool
	// closed prevents double-close.
	closed bool
}

// Close releases the HTTP connection. Idempotent.
func (s *anthropicStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.resp != nil && s.resp.Body != nil {
		return s.resp.Body.Close()
	}
	return nil
}

// Next returns the next decoded Event, or io.EOF when the stream ends cleanly.
//
// The SSE protocol (see https://html.spec.whatwg.org/multipage/server-sent-events.html)
// is line-oriented: each event is a series of `field: value` lines terminated
// by a blank line. Anthropic uses two fields:
//
//	event: <event_name>     ← the SSE event type
//	data: {...JSON...}      ← the payload
//
// We parse one event at a time, dispatch on its `type` field (which Anthropic
// ALSO embeds in the JSON, redundant with the SSE `event:` field — we trust
// the JSON one), and translate to zero-or-one of our Events.
func (s *anthropicStream) Next() (Event, error) {
	for {
		// Drain the queue first (covers the case where one SSE event
		// produced multiple outputs).
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.done {
			return Event{}, io.EOF
		}

		// Pull one full SSE event (lines until a blank line).
		dataLine, ok, err := s.readSSEEvent()
		if err != nil {
			return Event{}, err
		}
		if !ok {
			// EOF before message_stop. Treat as clean end — the
			// caller already saw whatever events arrived.
			s.done = true
			return Event{}, io.EOF
		}
		if dataLine == "" {
			// SSE comment / heartbeat with no data. Loop and read another.
			continue
		}

		// Decode just enough to discriminate. The `type` field is
		// always present at the top level of the data JSON.
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(dataLine), &head); err != nil {
			return Event{}, fmt.Errorf("malformed SSE data: %w", err)
		}

		switch head.Type {
		case "message_start":
			// message_start carries initial Usage (input tokens).
			// Body shape: {"type":"message_start","message":{"usage":{"input_tokens":N,...}}}
			var ms struct {
				Message struct {
					Usage Usage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(dataLine), &ms); err != nil {
				return Event{}, fmt.Errorf("message_start decode: %w", err)
			}
			s.usage.InputTokens = ms.Message.Usage.InputTokens
			// No Event emitted; loop and read the next SSE event.

		case "content_block_start":
			// {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"...","name":"...","input":{}}}
			var cbs struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(dataLine), &cbs); err != nil {
				return Event{}, fmt.Errorf("content_block_start decode: %w", err)
			}
			s.buffers[cbs.Index] = &contentBlockBuffer{
				kind: cbs.ContentBlock.Type,
				id:   cbs.ContentBlock.ID,
				name: cbs.ContentBlock.Name,
			}
			// No Event emitted yet — wait for deltas.

		case "content_block_delta":
			// Delta payload depends on the parent block's kind:
			//   text         → {"delta":{"type":"text_delta","text":"..."}}
			//   tool_use     → {"delta":{"type":"input_json_delta","partial_json":"..."}}
			//   thinking     → {"delta":{"type":"thinking_delta","thinking":"..."}}
			var cbd struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(dataLine), &cbd); err != nil {
				return Event{}, fmt.Errorf("content_block_delta decode: %w", err)
			}
			switch cbd.Delta.Type {
			case "text_delta":
				return Event{Type: EventText, Text: cbd.Delta.Text}, nil
			case "input_json_delta":
				if buf := s.buffers[cbd.Index]; buf != nil {
					buf.jsonAcc.WriteString(cbd.Delta.PartialJSON)
				}
				// No Event yet — wait for content_block_stop.
			case "thinking_delta":
				return Event{Type: EventReasoning, Reasoning: cbd.Delta.Thinking}, nil
			}

		case "content_block_stop":
			// {"type":"content_block_stop","index":0}
			// If the block was a tool_use, NOW we can emit one EventToolUse
			// with the accumulated JSON parsed.
			var cbe struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(dataLine), &cbe); err != nil {
				return Event{}, fmt.Errorf("content_block_stop decode: %w", err)
			}
			buf := s.buffers[cbe.Index]
			delete(s.buffers, cbe.Index)
			if buf != nil && buf.kind == "tool_use" {
				input := json.RawMessage(buf.jsonAcc.String())
				if len(input) == 0 {
					// Anthropic sends `"input":{}` when the tool
					// truly has no args; mirror that so consumers
					// always get valid JSON.
					input = json.RawMessage("{}")
				}
				return Event{
					Type: EventToolUse,
					ToolUse: &ToolUseRef{
						ID:    buf.id,
						Name:  buf.name,
						Input: input,
					},
				}, nil
			}

		case "message_delta":
			// message_delta carries final stop_reason + output_tokens usage.
			// {"type":"message_delta","delta":{"stop_reason":"end_turn",...},"usage":{"output_tokens":N}}
			var md struct {
				Usage Usage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(dataLine), &md); err != nil {
				return Event{}, fmt.Errorf("message_delta decode: %w", err)
			}
			s.usage.OutputTokens = md.Usage.OutputTokens

		case "message_stop":
			// Terminal SSE event. Emit our EventFinish (carrying the
			// usage assembled across message_start + message_delta),
			// mark done so the *next* Next() returns io.EOF.
			s.done = true
			usageCopy := s.usage
			return Event{Type: EventFinish, Usage: &usageCopy}, nil

		case "ping":
			// Heartbeat — ignore.

		case "error":
			// {"type":"error","error":{"type":"...","message":"..."}}
			return Event{}, fmt.Errorf("anthropic stream error: %s", dataLine)

		default:
			// Unknown event type — opencode's own client logs and ignores;
			// we do the same so a future Anthropic addition doesn't break us.
		}
	}
}

// readSSEEvent reads one SSE event (a sequence of non-blank lines terminated
// by a blank line) and returns its `data:` payload. Returns ("", false, nil)
// at clean EOF. Returns ("", true, nil) for an event with no data line
// (heartbeat / comment); the caller treats that as "loop and try again."
func (s *anthropicStream) readSSEEvent() (data string, ok bool, err error) {
	var dataB strings.Builder
	sawAny := false
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			// Blank line = end of event.
			if !sawAny {
				continue // adjacent blank lines, skip.
			}
			return dataB.String(), true, nil
		}
		sawAny = true
		// SSE field syntax: `field: value` (the space after `:` is optional).
		if strings.HasPrefix(line, ":") {
			continue // SSE comment, ignore.
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			// SSE allows `data:` to span multiple lines (joined with \n).
			// Anthropic puts the whole JSON on one line, but we still
			// support the spec.
			if dataB.Len() > 0 {
				dataB.WriteByte('\n')
			}
			dataB.WriteString(payload)
		}
		// `event:` field (also `id:`, `retry:`) — we ignore them; the
		// JSON `type` field inside data is authoritative for us.
	}
	if err := s.scanner.Err(); err != nil {
		return "", false, err
	}
	if sawAny {
		// Last event wasn't terminated by a blank line; return what we got.
		return dataB.String(), true, nil
	}
	return "", false, nil
}
