// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/provider/provider.ts (the BUNDLED_PROVIDERS map, L87-L150)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/provider/provider.ts#L87-L150
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s05 cares about this file:
//   This is the heart of opencode's Provider abstraction — a single map from
//   string keys ("@ai-sdk/anthropic", "@ai-sdk/openai", …) to dynamic-import
//   factories. At runtime, opencode resolves a (providerID, modelID) pair to
//   the matching factory, calls it with vendor-specific options (api key,
//   base URL, headers), and gets back a `BundledSDK` with one method:
//   `languageModel(modelId)`. That model is then handed to the Vercel AI
//   SDK's `streamText()` — which is what actually does the HTTP streaming.
//
//   For s05 we don't replicate the whole resolution machinery. We replicate
//   the *shape*: a Provider interface (one method, returns a Stream) with
//   one concrete implementation (AnthropicProvider). Phase G adds OpenAI,
//   Bedrock, etc. — each is its own factory func returning something that
//   satisfies Provider, and the call site stays unchanged.
//
// What we rebuilt in Go (s05):
//   - The string-keyed map of factories       → a Go interface + named structs
//                                                (AnthropicProvider, future OpenAIProvider)
//   - `BundledSDK.languageModel(modelId)`     → `Provider.Stream(ctx, Request) (Stream, error)`
//   - The AI SDK's `streamText()` SSE plumbing → our hand-rolled SSE reader
//                                                 in provider_anthropic.go
//   - `wrapSSE()` timeout cancellation         → `context.Context` deadline +
//                                                 `http.Client.Timeout`
//
// What we DID NOT rebuild yet (lives in later sessions or out-of-scope):
//   - Per-provider auth flows (OAuth, env-var fallback chain) — Phase G
//   - Custom model loaders (selectAzureLanguageModel, etc.) — Phase G
//   - Plugin-installed providers (the `custom()` map starting at L149)
//   - Provider-specific request transforms (`ProviderTransform`) — Phase G if needed
//   - Cost / pricing tables (`ModelsDev`)     — s14
//
// ---- begin upstream excerpt: packages/opencode/src/provider/provider.ts L87-L117 ----

type BundledSDK = {
  languageModel(modelId: string): LanguageModelV3
  // ↑ This is the surface every vendor's SDK presents to opencode. The Go
  //   equivalent is our Provider interface — one method that takes a model
  //   id (we put it on Request.Model) and returns something callable.
}

const BUNDLED_PROVIDERS: Record<string, () => Promise<(opts: any) => BundledSDK>> = {
  // ↑ Each value is a thunk: a function that returns a Promise of a *factory*.
  //   The thunk lets opencode dynamic-import only the SDKs the user actually
  //   configures (don't pay 30 npm packages of startup cost for a user who
  //   only uses Anthropic). Go has no equivalent to dynamic import; we
  //   instead register concrete provider structs at compile time and let
  //   the linker dead-code-eliminate the unused ones.
  "@ai-sdk/amazon-bedrock": () => import("@ai-sdk/amazon-bedrock").then((m) => m.createAmazonBedrock),
  "@ai-sdk/anthropic": () => import("@ai-sdk/anthropic").then((m) => m.createAnthropic),
  // ↑ This is the line s05 mirrors: createAnthropic returns a function that,
  //   given options ({apiKey, baseURL, headers}), returns a BundledSDK.
  //   Our `NewAnthropicProvider(apiKey, model)` IS that factory, just with
  //   the model name folded in as a default.
  "@ai-sdk/azure": () => import("@ai-sdk/azure").then((m) => m.createAzure),
  "@ai-sdk/google": () => import("@ai-sdk/google").then((m) => m.createGoogleGenerativeAI),
  "@ai-sdk/google-vertex": () => import("@ai-sdk/google-vertex").then((m) => m.createVertex),
  "@ai-sdk/google-vertex/anthropic": () =>
    import("@ai-sdk/google-vertex/anthropic").then((m) => m.createVertexAnthropic),
  "@ai-sdk/openai": () => import("@ai-sdk/openai").then((m) => m.createOpenAI),
  // ↑ Phase G adds an OpenAIProvider sibling to AnthropicProvider that
  //   satisfies the same Provider interface. The call site (`p.Stream(...)`)
  //   stays unchanged.
  "@ai-sdk/openai-compatible": () => import("@ai-sdk/openai-compatible").then((m) => m.createOpenAICompatible),
  "@openrouter/ai-sdk-provider": () => import("@openrouter/ai-sdk-provider").then((m) => m.createOpenRouter),
  "@ai-sdk/xai": () => import("@ai-sdk/xai").then((m) => m.createXai),
  "@ai-sdk/mistral": () => import("@ai-sdk/mistral").then((m) => m.createMistral),
  "@ai-sdk/groq": () => import("@ai-sdk/groq").then((m) => m.createGroq),
  "@ai-sdk/deepinfra": () => import("@ai-sdk/deepinfra").then((m) => m.createDeepInfra),
  "@ai-sdk/cerebras": () => import("@ai-sdk/cerebras").then((m) => m.createCerebras),
  "@ai-sdk/cohere": () => import("@ai-sdk/cohere").then((m) => m.createCohere),
  "@ai-sdk/gateway": () => import("@ai-sdk/gateway").then((m) => m.createGateway),
  "@ai-sdk/togetherai": () => import("@ai-sdk/togetherai").then((m) => m.createTogetherAI),
  "@ai-sdk/perplexity": () => import("@ai-sdk/perplexity").then((m) => m.createPerplexity),
  "@ai-sdk/vercel": () => import("@ai-sdk/vercel").then((m) => m.createVercel),
  "@ai-sdk/alibaba": () => import("@ai-sdk/alibaba").then((m) => m.createAlibaba),
  "gitlab-ai-provider": () => import("gitlab-ai-provider").then((m) => m.createGitLab),
  "@ai-sdk/github-copilot": () =>
    import("@opencode-ai/core/github-copilot/copilot-provider").then((m) => m.createOpenaiCompatible),
  "venice-ai-sdk-provider": () => import("venice-ai-sdk-provider").then((m) => m.createVenice),
  // ↑ 23 entries total. The string keys are npm package names — that's
  //   how opencode resolves "user wrote `provider: anthropic` in
  //   opencode.json" to "import @ai-sdk/anthropic and call createAnthropic".
  //   In Go we don't have npm; Phase G will build a similar map from
  //   short names ("anthropic", "openai", "bedrock") to factory funcs.
}

// ---- continued L119-L150: the custom-loader plumbing ----

type CustomModelLoader = (sdk: any, modelID: string, options?: Record<string, any>) => Promise<any>
type CustomVarsLoader = (options: Record<string, any>) => Record<string, string>
type CustomDiscoverModels = () => Promise<Record<string, Model>>
type CustomLoader = (provider: Info) => Effect.Effect<{
  autoload: boolean
  getModel?: CustomModelLoader
  vars?: CustomVarsLoader
  options?: Record<string, any>
  discoverModels?: CustomDiscoverModels
}>
// ↑ Custom loaders override BUNDLED_PROVIDERS for vendors that need
//   per-org logic (Azure deployments, Vertex regions, Bedrock IAM). For s05
//   we don't model this — our AnthropicProvider hardcodes the one shape
//   that matters. Phase G's per-provider option struct is where this
//   complexity would re-emerge if we wanted to, e.g., support Bedrock's
//   AWS SigV4 signing.

// ---- end upstream excerpt ----
//
// Reading map (in s05 order — later sessions read deeper):
//   1. provider.ts L87-L117 (BUNDLED_PROVIDERS)         — the string-key registry (this s05)
//   2. provider.ts L39-L85  (wrapSSE)                   — SSE timeout cancellation; we use ctx.Done() instead
//   3. session/llm.ts L100-L200                          — how streamText() is called from the loop (s06)
//   4. provider.ts L149+     (custom loaders)            — vendor-specific overrides (Phase G)
//   5. session/processor.ts L34-L150                     — where Events become Parts and tool calls dispatch (s10)
//
// The mental jump from upstream → s05 Go:
//   - `Record<string, () => Promise<factory>>`  → Go interface + concrete struct per vendor
//   - dynamic `import()` for lazy loading       → linker dead-code elimination + build tags
//   - `BundledSDK.languageModel(modelId)`       → `Provider.Stream(ctx, Request) (Stream, error)`
//   - `streamText()` returning AsyncIterable    → `Stream` interface with `Next() (Event, error)` and io.EOF
//   - per-provider `options: any`               → per-provider constructor args (NewAnthropicProvider…)
// What stays identical: the wire bytes Anthropic sees, the events the consumer
// receives, the contract that "providerless" code (the loop, the reducer) has
// no idea which vendor it's talking to.
