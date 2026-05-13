// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/llm.ts (excerpt — header + Service def)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts
//
// Why s01 cares about this file:
//   This file IS the LLM call layer. Everything else in opencode either
//   feeds into it (Agent.system prompt, Tool registry) or reads its
//   output (Session storage, Cost tracking). The 30 lines below are
//   the smallest sub-shape that exists in opencode's version too.
//
// What we rebuilt in Go:
//   - streamText(...) → Provider.CreateMessage(ctx, req)   [non-streaming for now]
//   - Effect.gen → plain func returning (T, error)
//   - Layer-injected Provider.Service → Provider interface in provider.go
//
// What we DID NOT rebuild yet (later sessions):
//   - tool calling   → s03 (registry) + s10 (loop)
//   - streaming SSE  → s06
//   - retry / abort  → s14
//   - permissions    → s04
//
// ---- begin upstream excerpt ----

import { streamText, type ToolSet } from "ai"
import { Effect, Layer, Schema } from "effect"
import { Provider } from "../provider/provider"
import { Permission } from "../permission/permission"
import { Tool } from "../tool/tool"

export namespace LLM {
  export class Service extends Effect.Service<Service>()("LLM", {
    effect: Effect.gen(function* () {
      const provider = yield* Provider.Service
      const permission = yield* Permission.Service

      return {
        stream: (input: StreamInput) =>
          Effect.gen(function* () {
            const model = yield* provider.model(input.providerID, input.modelID)

            // Build the merged system prompt: agent.system + provider-specific
            // additions + user-config system + per-call system.
            const system = [
              input.agent.system,
              SystemPrompt.provider(input.providerID),
              input.config.system,
              input.system,
            ].filter(Boolean).join("\n\n")

            // Filter the tool set down to what the agent's permission ruleset
            // allows in this exact call site.
            const tools = yield* permission.filterTools(
              input.tools,
              input.agent.permissions,
            )

            return streamText({
              model,
              system,
              messages: input.messages,
              tools,
              toolChoice: input.toolChoice,
              maxRetries: 3,
              abortSignal: input.signal,
              // ... + telemetry hooks ...
            })
          }),
      }
    }),
  }) {}
}

// ---- end upstream excerpt ----
//
// Reading order (do these in s01 only — later sessions read deeper):
//   1. Provider.Service (provider/provider.ts L87-L150) — how (providerID, modelID) → LanguageModelV3
//   2. streamText source from ai-sdk (skim only — too big to study)
//   3. The Permission.filterTools call: that's the code path you'll see again in s10
//
// For s01 you ONLY need to internalize: "the LLM call returns a stream of
// events; the wire format is messages[] in / events out". Everything else is
// a future session.
