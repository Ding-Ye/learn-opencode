package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the merged result of (project file ⊕ user file ⊕ env overrides)
// — the same shape opencode's `config.ts` exports as `Info`. We trim heavily:
// upstream's Info has ~30 fields; we keep the seven that downstream sessions
// will actually consume. Each kept field has a clear later-session owner:
//
//   - Provider              → s09 picks the model when constructing an Agent.
//   - Agents                → s09 turns each AgentConfig into an Agent.Info.
//   - Permissions           → s10's tool loop calls evaluate(...) against this.
//   - Instructions          → s11's system-prompt builder appends each file.
//   - LSP                   → s13 spawns one language server per entry.
//   - MCP                   → s12 spawns one MCP child per entry.
//   - Skills                → s11 walks each path looking for SKILL.md.
//
// Anything not on that list (compaction tuning, share mode, autoupdate,
// telemetry, watcher ignore lists, formatter selection) lives in upstream
// but not here — adding them later is `+1 field + 1 JSON tag`, no merge
// logic changes because mergeConfigs is structural.
type Config struct {
	Provider     ProviderConfig       `json:"provider"`
	Agents       []AgentConfig        `json:"agents"`
	Permissions  []Rule               `json:"permissions"`
	Instructions []string             `json:"instructions"`
	LSP          map[string]LSPConfig `json:"lsp"`
	MCP          []MCPConfig          `json:"mcp"`
	Skills       map[string]string    `json:"skills"`
}

// ProviderConfig is the (provider, model) pair the agent will use by
// default. Upstream's `Info.model` is a single string `"anthropic/claude-..."`
// that gets split by `/`; we keep the two halves separately so an env
// override can target one without re-parsing.
type ProviderConfig struct {
	ProviderID string `json:"provider"`
	ModelID    string `json:"model"`
}

// AgentConfig is the JSON-side declaration of one agent. s09 will turn this
// into a runtime `Agent` (system prompt resolved, model resolved against
// Provider, permissions cascaded). Permissions here is the agent's *own*
// rules — s09 cascades them after the global Permissions[] above.
type AgentConfig struct {
	Name        string `json:"name"`
	Mode        string `json:"mode"`  // "primary" | "subagent" | "all"
	Model       string `json:"model"` // optional override; falls back to Provider.ModelID
	Permissions []Rule `json:"permissions"`
}

// LSPConfig is one language server entry. Minimal: command + args.
// Upstream's LSP config has filetypes / initialization options / disabled
// flag etc; we surface just the spawn fields because s13 will rebuild the
// rest when it actually wires gopls.
type LSPConfig struct {
	Command []string `json:"command"`
	Disable bool     `json:"disable"`
}

// MCPConfig is one MCP server entry. Two transports exist upstream: stdio
// (spawn a child process, talk JSON-RPC over its stdin/stdout) and remote
// (HTTP + SSE). We carry both shapes' fields with omitempty so a single
// struct serves both. s12 fills in the spawn / connect logic.
type MCPConfig struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // "stdio" | "remote"
	Command []string `json:"command,omitempty"`
	URL     string   `json:"url,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"` // pointer so absent ≠ false
}

// Load is the public entry point. It runs three stages in deterministic
// order, then deep-merges the results:
//
//  1. User config ←     <homeDir>/.opencode/opencode.json{,c}
//  2. Project config ←  walk up from cwd to find .opencode/opencode.json{,c}
//  3. Env overrides ←   OPENCODE_PROVIDER / OPENCODE_MODEL
//
// Merge order: project overrides user (project is closer to the user's
// intent — if you're sitting in `~/projects/foo`, foo's `.opencode/` wins
// over the global one). Env overrides win over both — they're the runtime
// "do this one different thing" knob.
//
// `Instructions[]` is the one field that *concatenates* (user-first, then
// project) instead of being replaced. Upstream's `mergeConfigConcatArrays`
// also uniques the result; we do the same so a duplicate path only loads
// once. All other arrays follow the structural override semantics: a
// project `permissions[]` of length 1 *replaces* the user's of length 5.
// (Upstream behaves the same — `mergeDeep` from remeda overwrites arrays
// at the leaf, with `instructions` the explicit exception.)
//
// Returns an error if a discovered config file fails to parse. Missing
// files are NOT errors — that's the "no config" case, valid in tests.
func Load(cwd, homeDir string, env map[string]string) (*Config, error) {
	user, err := loadOptional(userConfigPaths(homeDir))
	if err != nil {
		return nil, fmt.Errorf("user config: %w", err)
	}

	var project Config
	if path, ok := findProjectConfig(cwd); ok {
		p, perr := loadOne(path)
		if perr != nil {
			return nil, fmt.Errorf("project config %s: %w", path, perr)
		}
		project = p
	}

	merged := mergeConfigs(user, project)
	applyEnvOverrides(&merged, env)
	return &merged, nil
}

// loadOptional iterates a list of candidate paths and returns the first one
// that exists & parses. Used for the user config (where the candidate list
// is `[opencode.jsonc, opencode.json]`). Missing → empty Config, no error;
// present-but-malformed → error so the user sees the JSON parse failure.
func loadOptional(paths []string) (Config, error) {
	for _, p := range paths {
		if !fileExists(p) {
			continue
		}
		c, err := loadOne(p)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", p, err)
		}
		return c, nil
	}
	return Config{}, nil
}

// loadOne reads + parses a single JSON / JSONC file. We handle .jsonc by
// stripping comments before json.Unmarshal — opencode does the same via
// jsonc-parser, but since we don't need the cursor-position machinery (no
// editor integration in s08), a regex-and-state-machine strip is enough.
//
// Why allow JSONC at all? Because opencode's config files in the wild
// have `// comments`, and importing one as the s08 "user config" should
// just work without the user pre-stripping. The strip is dumb but safe:
// it preserves comments inside string literals, so `"key": "// not a comment"`
// stays intact.
func loadOne(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	// Strip JSONC comments if the file extension implies them, OR if it's
	// .json but contains a `//` outside a string. Conservative: if strip
	// fails to produce valid JSON we'll surface that as the next error.
	if strings.HasSuffix(path, ".jsonc") || containsLikelyComment(raw) {
		raw = stripJSONC(raw)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// containsLikelyComment is the cheap heuristic: scan for `//` or `/*`
// outside obvious string boundaries. False positives are fine — they just
// trigger an unnecessary strip pass on a clean .json file. False negatives
// (a real comment that we don't strip) would surface as the json.Unmarshal
// error, which is also fine — the user sees the parse failure and can
// rename to `.jsonc` to opt in.
func containsLikelyComment(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw)-1; i++ {
		c := raw[i]
		if c == '"' && (i == 0 || raw[i-1] != '\\') {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '/' && (raw[i+1] == '/' || raw[i+1] == '*') {
			return true
		}
	}
	return false
}

// stripJSONC removes `// line comments` and `/* block comments */` from a
// JSON byte slice. Preserves string contents (so `"//"` inside a value is
// untouched). Hand-rolled state machine: 4 states (default, in-string,
// in-line-comment, in-block-comment). 30 lines of code, no dep, exactly
// what opencode does via jsonc-parser at a higher level of polish.
func stripJSONC(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	i := 0
	for i < len(raw) {
		c := raw[i]
		// String literal: copy until unescaped closing quote.
		if c == '"' {
			out = append(out, c)
			i++
			for i < len(raw) {
				out = append(out, raw[i])
				if raw[i] == '\\' && i+1 < len(raw) {
					out = append(out, raw[i+1])
					i += 2
					continue
				}
				if raw[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		// Line comment: skip to newline (preserve the newline itself so
		// row/column-based error messages aren't shifted).
		if c == '/' && i+1 < len(raw) && raw[i+1] == '/' {
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment: skip to closing */.
		if c == '/' && i+1 < len(raw) && raw[i+1] == '*' {
			i += 2
			for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
				if raw[i] == '\n' {
					out = append(out, '\n') // preserve line numbers
				}
				i++
			}
			i += 2 // skip closing */
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

// mergeConfigs deep-merges `override` onto `base` and returns the result.
// The semantics, per upstream's `mergeConfigConcatArrays`:
//
//   - Scalars (Provider.ModelID, etc.):        override wins if non-empty.
//   - Maps (LSP, Skills):                      union; override wins on key clash.
//   - Slices except Instructions:              override wins if non-empty.
//   - Instructions:                            base ++ override, then dedup
//                                              (set semantics, preserves order).
//
// The "non-empty wins" rule for scalars is upstream's `mergeDeep` behavior:
// `undefined` in TS lets the base value through. In Go we use the zero
// value (`""`, `nil`) as the equivalent — a config that didn't set a
// field doesn't override one that did.
func mergeConfigs(base, override Config) Config {
	out := base

	// Scalar / nested-struct: ProviderConfig — override per-field if non-empty.
	if override.Provider.ProviderID != "" {
		out.Provider.ProviderID = override.Provider.ProviderID
	}
	if override.Provider.ModelID != "" {
		out.Provider.ModelID = override.Provider.ModelID
	}

	// Slices: override-wins-if-non-empty for everything except Instructions.
	if len(override.Agents) > 0 {
		out.Agents = override.Agents
	}
	if len(override.Permissions) > 0 {
		out.Permissions = override.Permissions
	}
	if len(override.MCP) > 0 {
		out.MCP = override.MCP
	}

	// Instructions: concat base ++ override, dedup preserving first occurrence.
	// Matches upstream's `Array.from(new Set([...target.instructions, ...source.instructions]))`.
	if len(base.Instructions) > 0 || len(override.Instructions) > 0 {
		out.Instructions = dedupStrings(append(append([]string{}, base.Instructions...), override.Instructions...))
	}

	// Maps: shallow-union with override winning on key collision.
	out.LSP = mergeStringMap(base.LSP, override.LSP)
	out.Skills = mergeSimpleStringMap(base.Skills, override.Skills)

	return out
}

// mergeStringMap merges two map[string]LSPConfig; override wins on key
// collision. Returns a fresh map so callers can mutate without aliasing
// base. nil inputs are treated as empty.
func mergeStringMap(base, override map[string]LSPConfig) map[string]LSPConfig {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]LSPConfig, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// mergeSimpleStringMap is mergeStringMap for map[string]string. Same
// semantics; separate body because Go generics on map values would add
// noise the teaching session doesn't need.
func mergeSimpleStringMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// dedupStrings preserves first-occurrence order, drops duplicates. Used
// only for the Instructions concat path. O(n) with a temp set; the slice
// is tiny (5–20 entries in practice) so the allocation is fine.
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// applyEnvOverrides mutates Config in place with env-var overrides. Two
// vars are honored, mirroring opencode's runtime knobs:
//
//   - OPENCODE_PROVIDER → Config.Provider.ProviderID
//   - OPENCODE_MODEL    → Config.Provider.ModelID
//
// These win over both files (project + user) because they're the most
// transient layer — a `OPENCODE_MODEL=claude-3-opus go run .` should
// take effect for that one invocation without editing JSON. Empty values
// are ignored (treat unset and "" the same; the env map this fn takes
// can come from os.Environ-via-helper or a test fixture).
//
// Future env vars get added here; the convention is `OPENCODE_<UPPER>`.
func applyEnvOverrides(c *Config, env map[string]string) {
	if v := env["OPENCODE_PROVIDER"]; v != "" {
		c.Provider.ProviderID = v
	}
	if v := env["OPENCODE_MODEL"]; v != "" {
		c.Provider.ModelID = v
	}
}

// EnvFromOS is a convenience the demo uses to grab the relevant env vars
// from the live process. Tests construct their own map directly so they
// can pin OPENCODE_MODEL to a specific value without touching the
// process-wide env (which would race with parallel tests).
func EnvFromOS() map[string]string {
	keys := []string{"OPENCODE_PROVIDER", "OPENCODE_MODEL"}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// jsonMarshal / jsonUnmarshal are tiny shims used by Rule's custom Marshal/
// UnmarshalJSON to avoid importing encoding/json in permission.go. (Also
// keeps that file's import set clean for readers focusing on the rule
// shape.) The only reason this file exposes them is the cross-file usage.
func jsonMarshal(v any) ([]byte, error)         { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error    { return json.Unmarshal(data, v) }
