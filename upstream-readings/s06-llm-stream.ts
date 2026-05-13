// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/session/llm.ts (the streamText prep + invocation, L100-L200)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/llm.ts#L100-L200
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s06 cares about this file:
//   This is the call site that turns "we have a Provider and a Request" into
//   "we have a Stream of Events to consume." opencode's `Service.run()` (the
//   Effect-typed function this excerpt lives in) does three things in order:
//
//     1. Compose the system prompt from N sources (agent prompt, provider
//        defaults, custom user system, user-message-attached system).
//     2. Build the `params` payload (temperature, topP, maxOutputTokens,
//        provider-specific options) — letting plugins rewrite it via the
//        `chat.params` hook.
//     3. Call `streamText()` from the AI SDK with all of the above; return
//        the `result` whose `result.fullStream` is the AsyncIterable of
//        events the rest of the codebase reduces.
//
//   For s06 we mirror only step 3's *consumer* side: given a Stream we got
//   from somewhere, assemble its Events into a Message of Parts. Steps 1
//   and 2 are out of scope here — s09 (agent registry) handles the system
//   prompt cascade; the per-vendor options dictionary is hard-coded out of
//   our Request struct (we keep only the cross-vendor fields).
//
//   The annotated section below is the *prep* — system prompt assembly,
//   variant resolution, options merging — because reading it is what makes
//   you appreciate why our Request struct is so small. opencode's runtime
//   builds a 6-layer-deep nested options blob; we squash it to one flat
//   struct because s06 is teaching the assembler, not the resolver.
//
// What we rebuilt in Go (s06):
//   - The `result.fullStream` AsyncIterable consumer    → `Loop.Consume(ctx, req)`
//   - opencode's per-Event reducer (in processor.ts)    → our switch-on-Event in loop.go
//   - "concat adjacent text deltas" semantics            → our `trailing` boundary tracker
//   - "tool_use input is buffered until content_block_stop" → kept in s05;
//                                                          Loop receives EventToolUse
//                                                          already buffered
//   - context cancellation                               → `ctx.Err()` check before Next()
//
// What we DID NOT rebuild yet (lives in later sessions):
//   - System prompt cascade (agent + provider + user)    — s09 (agent registry)
//   - Plugin `chat.params` / `chat.headers` hooks        — out of scope
//   - Tool dispatch on EventToolUse                      — s10 (tool loop)
//   - Permission check before tool execution             — s04 + s10
//   - Snapshot before stream + diff after                — s07 (storage) + s10
//   - "Doom loop" 3-retry cap                            — s14 (recovery)
//   - Persistence to SessionMessageTable                 — s07
//
// ---- begin upstream excerpt: packages/opencode/src/session/llm.ts L100-L143 ----

      // TODO: move this to a proper hook
      const isOpenaiOauth = item.id === "openai" && info?.type === "oauth"

      const system: string[] = []
      system.push(
        [
          // use agent prompt otherwise provider prompt
          ...(input.agent.prompt ? [input.agent.prompt] : SystemPrompt.provider(input.model)),
          // ↑ Agent's custom prompt wins over the provider's default. s09 will
          //   wire this — for s06 we don't have an agent yet, so the s06 demo
          //   uses an empty system field on Request.
          // any custom prompt passed into this call
          ...input.system,
          // ↑ "Custom prompt this call" is the per-call override the caller
          //   passes to streamText. Our Request.System is the equivalent — one
          //   string, not an array, because s06 doesn't compose layers.
          // any custom prompt from last user message
          ...(input.user.system ? [input.user.system] : []),
          // ↑ Per-message system override. Out of scope for s06; s09 may add.
        ]
          .filter((x) => x)
          .join("\n"),
      )

      const header = system[0]
      yield* plugin.trigger(
        "experimental.chat.system.transform",
        { sessionID: input.sessionID, model: input.model },
        { system },
      )
      // ↑ Plugin hook: lets a user plugin rewrite the system prompt array
      //   before send. Out of scope for s06 — we have no plugin layer yet.
      // rejoin to maintain 2-part structure for caching if header unchanged
      if (system.length > 2 && system[0] === header) {
        const rest = system.slice(1)
        system.length = 0
        system.push(header, rest.join("\n"))
      }
      // ↑ Anthropic prompt-caching only honors the first system block. opencode
      //   keeps that block stable so cache hits across turns; everything else
      //   collapses into block #2. Phase G / s14 cost work may revisit.

      const variant =
        !input.small && input.model.variants && input.user.model.variant
          ? input.model.variants[input.user.model.variant]
          : {}
      // ↑ "Variants" let one model id (claude-sonnet-4-5) have e.g. a
      //   high-effort and low-effort version with different temperature. We
      //   don't model variants; the caller picks one Model string.
      const base = input.small
        ? ProviderTransform.smallOptions(input.model)
        : ProviderTransform.options({
            model: input.model,
            sessionID: input.sessionID,
            providerOptions: item.options,
          })
      // ↑ ProviderTransform.options returns a per-provider options blob
      //   (Anthropic gets {anthropic: {...}}; OpenAI gets {openai: {...}}).
      //   Our s06 Request has no `Options` field — provider-specific tuning
      //   is one of the things s05's interface deliberately drops to keep
      //   the cross-vendor surface minimal. Phase G adds it back as an
      //   AnthropicOptions / OpenAIOptions struct on the concrete provider.
      const options = mergeOptions(mergeOptions(mergeOptions(base, input.model.options), input.agent.options), variant)
      // ↑ Four-way deep merge: base → model defaults → agent overrides → variant.
      //   Each later layer beats the previous. We have ZERO of this in s06 —
      //   our Request is a flat 6-field struct. The mental model upgrade
      //   you should make: this 4-layer merge is what `Provider.Stream`
      //   abstracts AWAY from the consumer (the Loop). The Loop doesn't
      //   know or care what knobs were passed; it only cares that Events
      //   come back. That's the whole win of the abstraction.

// ---- end upstream excerpt ----
//
// Reading map (in s06 order — later sessions read deeper):
//   1. session/llm.ts L100-L143 (system + options assembly)   — this excerpt (s06)
//   2. session/llm.ts L336-L415 (the streamText() call)        — what s05 hand-rolls in SSE
//   3. session/processor.ts L34-L150 (Event → Part reducer)    — what our Loop replaces (s10)
//   4. session/llm.ts L418-L432 (Stream.fromAsyncIterable)     — what our Stream interface mirrors
//   5. session/processor.ts (tool dispatch on tool_use)         — s10
//
// The mental jump from upstream → s06 Go:
//   - 6-layer system prompt cascade           → one flat Request.System string
//   - 4-way merged provider options blob      → ZERO options, vendor sets defaults
//   - Plugin-rewritten params + headers       → none, Provider.Stream is opaque
//   - Effect-typed `streamText()` result      → our Stream interface (Next + Close)
//   - AsyncIterable consumer in processor.ts  → for-loop over stream.Next() in Loop.Consume
//   - "is this stream still mid-stream"       → ctx.Err() check before each Next()
//
// What stays identical: the contract between Stream producer and Stream
// consumer. The producer (s05 Anthropic SSE parser, or a fake) emits Events.
// The consumer (s06 Loop) assembles them into a Message of Parts. Neither
// side knows what's on the other end — and that's why the Loop in s06 is
// only ~150 lines: it doesn't have to care about Anthropic, the AI SDK,
// system prompts, plugin params, options merging, or anything else opencode's
// `Service.run()` is doing in the lines above. It just consumes Events.
