# s01 — minimum-loop

The smallest possible Go translation of opencode's LLM call layer.

- **One file does the work** (`provider.go`): a `Provider` interface, an `AnthropicProvider` concrete impl, and the JSON wire types (`Message`, `ContentBlock`, `CreateMessageRequest`, `CreateMessageResponse`, `Usage`).
- **`main.go`** is 50 lines: read flags, hit the API once, print text.
- **No tools, no streaming, no permissions, no session storage.** Every later session adds exactly one of those.

## Run

```
export ANTHROPIC_API_KEY=sk-ant-...
go run . hello in three words
```

## Test (offline)

```
go test ./...
```

The 4 tests use `httptest.NewServer` to stub Anthropic — no network, no API key needed.

## What this maps to upstream

| This file | Upstream file |
|---|---|
| `provider.go` Provider interface | `packages/opencode/src/provider/provider.ts` (only the model() factory) |
| `provider.go` Message/ContentBlock | `packages/opencode/src/session/message-v2.ts` (Text part only) |
| `main.go` | `packages/opencode/src/session/llm.ts` (streamText, minus the streaming) |

See `docs/zh/s01-minimum-loop.md` and `docs/en/s01-minimum-loop.md` for the long-form walkthrough.
