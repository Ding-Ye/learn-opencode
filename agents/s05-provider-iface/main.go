package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// main is a hand-runnable demo of the Provider streaming interface:
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run . hello in three words
//
// Builds a Request, calls AnthropicProvider.Stream, then loops over the
// Stream printing each Event as it arrives. This is the smallest possible
// example of the s05 -> s06 -> s10 chain: s06 will swap this print loop for
// a Part-aggregating reducer, s10 will swap it again for one that dispatches
// tool calls and feeds their results back. The Provider call site stays the
// same.
func main() {
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-5-20250929"), "Anthropic model id")
	system := flag.String("system", "You are a helpful coding assistant. Be terse.", "system prompt")
	maxTokens := flag.Int("max-tokens", 256, "max output tokens")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(),
			"usage: s05 [-model ID] [-system 'prompt'] [-max-tokens N] <prompt-words...>\n\n"+
				"  Streams one assistant response from Anthropic, printing each Event as it\n"+
				"  arrives. Demonstrates the s05 Provider interface: same wire format as s01,\n"+
				"  but pull-based via Stream.Next() instead of one big blocking response.\n\n"+
				"Example:\n"+
				"  s05 hello in three words")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}

	// Notice: `var p Provider = ...` — we deliberately bind to the
	// interface, not the concrete type, to demonstrate that nothing below
	// this line knows it's talking to Anthropic specifically. Phase G's
	// OpenAIProvider drops in here unchanged.
	var p Provider = NewAnthropicProvider(apiKey, *model)

	stream, err := p.Stream(context.Background(), Request{
		MaxTokens: *maxTokens,
		System:    *system,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}},
		},
	})
	if err != nil {
		log.Fatalf("provider error: %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatalf("stream error: %v", err)
		}
		switch ev.Type {
		case EventText:
			// Print without newline so adjacent deltas concatenate
			// into the natural prose the model is generating.
			fmt.Print(ev.Text)
		case EventToolUse:
			fmt.Printf("\n[tool_use] %s(%s) input=%s\n", ev.ToolUse.Name, ev.ToolUse.ID, string(ev.ToolUse.Input))
		case EventReasoning:
			fmt.Fprintf(os.Stderr, "[reasoning] %s", ev.Reasoning)
		case EventFinish:
			fmt.Println() // terminate the prose line
			fmt.Fprintf(os.Stderr, "[s05] tokens: in=%d out=%d\n", ev.Usage.InputTokens, ev.Usage.OutputTokens)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
