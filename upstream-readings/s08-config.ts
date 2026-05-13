// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/config/config.ts (mergeConfig + load + normalize, L49-L110)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/config/config.ts#L49-L110
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s08 cares about this file:
//   This is the *exact* deep-merge + array-concat code we Go-port. Three
//   functions in particular drive everything our s08 config.go does:
//
//     1. `mergeConfig(target, source)` — wraps remeda's `mergeDeep` so
//        scalars / nested objects override per-field. Our Go equivalent is
//        `mergeConfigs(base, override)` in config.go: same per-field
//        non-empty-wins semantics, written out by hand because Go has no
//        equivalent of remeda's structural deep-merge.
//
//     2. `mergeConfigConcatArrays(target, source)` — the *concat* exception
//        for `instructions`. Calls mergeConfig first, then post-processes
//        `instructions` to be the deduped union of both. Our Go equivalent
//        is the `Instructions` branch in `mergeConfigs` plus `dedupStrings`.
//        Identical semantics: user-first, project-after, deduped via Set.
//
//     3. `normalizeLoadedConfig(data, source)` — strips legacy `theme` /
//        `keybinds` / `tui` keys (moved to a separate `tui.json` file).
//        We don't need this for s08 because we don't carry those fields
//        in our trimmed `Config` at all — the keys are simply ignored by
//        `json.Unmarshal` since they're not in the struct.
//
//   The `Info` schema (L120-L292 in config.ts) is the full ~30-field config
//   surface. Our Go `Config` keeps 7: Provider, Agents, Permissions,
//   Instructions, LSP, MCP, Skills. Each kept field has a concrete
//   downstream consumer in s09-s14; the dropped fields will be added back
//   field-by-field as their owning sessions land (no merge logic changes —
//   the merge is structural).
//
// What we rebuilt in Go (s08):
//   - mergeConfig (deep-merge wrapper)         → mergeConfigs in config.go
//   - mergeConfigConcatArrays (concat exception) → Instructions branch + dedupStrings
//   - Info schema (Provider / Permissions / etc.) → Config struct, 7 fields
//   - normalizeLoadedConfig                     → not needed (unknown fields ignored)
//   - jsonc-parser strip                        → stripJSONC state machine (no dep)
//   - paths.directories (walk-up search)        → findProjectConfig in paths.go
//   - Flag.OPENCODE_CONFIG_DIR                  → DefaultHomeDir in paths.go
//
// What we DID NOT rebuild yet (lives in later sessions or out of scope):
//   - ConfigVariable.substitute (env-var expansion in JSON values) — s11
//   - resolveLoadedPlugins (plugin spec resolution)                — out of scope
//   - Effect-typed Service / layer machinery                       — Go uses plain func
//   - Schema.parse runtime validation (effect/Schema)              — Go uses struct tags
//   - $schema field auto-injection on save                         — read-only in s08
//   - Auth / Account / Npm Service deps                            — out of scope
//   - patchJsonc (in-place edit-with-formatting)                   — read-only in s08
//   - File watcher / live config reload                            — s_full integration
//
// The 30-60 lines below are the heart of opencode's config layer — three
// small functions doing structurally exactly what our Go module does, just
// expressed through remeda's `mergeDeep` / `Set` rather than hand-rolled.

// Custom merge function that concatenates array fields instead of replacing them
// Keep remeda's deep conditional merge type out of hot config-loading paths;
// TS profiling showed it dominates here.
function mergeConfig(target: Info, source: Info): Info {
  return mergeDeep(target, source) as Info
}

function mergeConfigConcatArrays(target: Info, source: Info): Info {
  const merged = mergeConfig(target, source)
  if (target.instructions && source.instructions) {
    // Set semantics: order preserved (Set in JS iterates in insertion
    // order), duplicates dropped. Our Go dedupStrings does the same:
    // map-as-set + result slice in first-occurrence order.
    merged.instructions = Array.from(new Set([...target.instructions, ...source.instructions]))
  }
  return merged
}

function normalizeLoadedConfig(data: unknown, source: string) {
  if (!isRecord(data)) return data
  const copy = { ...data }
  // The legacy keys: opencode used to carry `theme` / `keybinds` / `tui`
  // inline in opencode.json; they've since moved to a sibling tui.json.
  // The warn-and-strip preserves backward-compat without breaking startup.
  // Our Go config doesn't include these fields at all — encoding/json
  // silently drops unknown keys, so this normalization step would be a
  // no-op for us. We don't carry it.
  const hadLegacy = "theme" in copy || "keybinds" in copy || "tui" in copy
  if (!hadLegacy) return copy
  delete copy.theme
  delete copy.keybinds
  delete copy.tui
  log.warn("tui keys in opencode config are deprecated; move them to tui.json", { path: source })
  return copy
}

// The end-to-end load Effect for one config file (lives further down in
// the file, ~L376-L404). Stripped of its Effect wrapper, this is what
// happens for each file we discover:
const loadConfig = Effect.fnUntraced(function* (
  text: string,
  options: { path: string } | { dir: string; source: string },
) {
  const source = "path" in options ? options.path : options.source
  // Step 1: substitute ${ENV_VAR} placeholders in the raw text. We don't
  // implement this in s08 — it's s11's territory (skill-discovery uses
  // similar templating) and most projects don't use it.
  const expanded = yield* Effect.promise(() =>
    ConfigVariable.substitute(
      "path" in options ? { text, type: "path", path: options.path } : { text, type: "virtual", ...options },
    ),
  )
  // Step 2: strip JSONC comments. We do this in stripJSONC (config.go)
  // instead of importing jsonc-parser, because the only thing s08 needs
  // is comment stripping — none of jsonc-parser's edit / format / cursor
  // machinery applies to a read-only loader.
  const parsed = ConfigParse.jsonc(expanded, source)
  // Step 3: schema-validate. Effect's Schema runtime checker. Our Go
  // version is `json.Unmarshal` into a typed struct — fields not on the
  // struct are silently dropped, fields with the wrong type fail with a
  // typed error. Lighter weight; sufficient for s08.
  const data = ConfigParse.schema(Info, normalizeLoadedConfig(parsed, source), source)
  if (!("path" in options)) return data
  // Steps 4-5: plugin resolution and $schema injection — not in s08.
  yield* Effect.promise(() => resolveLoadedPlugins(data, options.path))
  if (!data.$schema) {
    data.$schema = "https://opencode.ai/config.json"
    const updated = text.replace(/^\s*\{/, '{\n  "$schema": "https://opencode.ai/config.json",')
    yield* fs.writeFileString(options.path, updated).pipe(Effect.catch(() => Effect.void))
  }
  return data
})

// END EXCERPT — full file is 1500+ lines including the entire `Info`
// Schema declaration (L120-L292). The merge rules above are the part
// s08 ports verbatim.
