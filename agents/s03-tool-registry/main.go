package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// main is a hand-runnable demo of the Tool registry round-tripping a real
// dispatch end-to-end:
//
//	go run .
//
// (1) builds an empty Registry, (2) registers the two built-ins, (3) prints
// the LLM-facing tool_schemas JSON (this is exactly what s05's Provider will
// later splice into Anthropic's `tools` request field), (4) dispatches the
// echo tool with a sample LLM-shaped input, (5) prints the dispatch output.
func main() {
	reg := NewRegistry()
	if err := reg.Register(EchoTool{}); err != nil {
		fmt.Fprintln(os.Stderr, "register echo:", err)
		os.Exit(1)
	}
	if err := reg.Register(NowTool{}); err != nil {
		fmt.Fprintln(os.Stderr, "register now:", err)
		os.Exit(1)
	}

	fmt.Println("--- registered tools ---")
	for _, name := range reg.Names() {
		fmt.Println(" ", name)
	}

	schemas, err := reg.ToolSchemas()
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemas:", err)
		os.Exit(1)
	}
	wire, err := json.MarshalIndent(schemas, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal schemas:", err)
		os.Exit(1)
	}
	fmt.Println("\n--- tool_schemas (the JSON the LLM sees) ---")
	fmt.Println(string(wire))

	// Pretend the LLM emitted: tool_use{name:"echo", input:{"text":"hi from s03"}}.
	// In s10's loop this is exactly what arrives off the SSE stream.
	echoInputJSON := json.RawMessage(`{"text":"hi from s03"}`)
	out, err := reg.Dispatch(context.Background(), "echo", echoInputJSON)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch echo:", err)
		os.Exit(1)
	}
	fmt.Println("\n--- dispatch echo({\"text\":\"hi from s03\"}) ---")
	fmt.Println(" ", out)

	// And `now` with empty input — exercises the "no args" path.
	out, err = reg.Dispatch(context.Background(), "now", json.RawMessage(`{}`))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch now:", err)
		os.Exit(1)
	}
	fmt.Println("\n--- dispatch now({}) ---")
	fmt.Println(" ", out)
}
