package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-5-20250929"), "Anthropic model id")
	system := flag.String("system", "You are a helpful coding assistant. Be terse.", "system prompt")
	maxTokens := flag.Int("max-tokens", 1024, "max output tokens")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(),
			"usage: s01 [-model ID] [-system 'prompt'] [-max-tokens N] <prompt-words...>\n\n"+
				"  Sends one message to Anthropic and prints the assistant text. No tools, no streaming.\n"+
				"  Requires ANTHROPIC_API_KEY in env. This is the smallest possible analogue of opencode's\n"+
				"  packages/opencode/src/session/llm.ts streamText() — minus streaming, minus tools.\n\n"+
				"Example:\n"+
				"  s01 hello in three words")
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

	p := NewAnthropicProvider(apiKey, *model)

	resp, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		MaxTokens: *maxTokens,
		System:    *system,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}},
		},
	})
	if err != nil {
		log.Fatalf("provider error: %v", err)
	}

	for _, b := range resp.Content {
		if b.Type == "text" {
			fmt.Println(b.Text)
		}
	}
	fmt.Fprintf(os.Stderr, "[s01] tokens: in=%d out=%d stop=%s\n",
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StopReason)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
