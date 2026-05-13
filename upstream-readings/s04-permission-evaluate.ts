// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/permission/evaluate.ts (the whole file — 16 lines)
//        + util/wildcard.ts L3-L19 (the matcher evaluate.ts depends on)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/permission/evaluate.ts
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s04 cares about this file:
//   This IS the whole permission evaluator. 16 lines. Every later session
//   that needs to ask "may the agent do X to Y?" funnels through here:
//     - s09 (agent-registry) — caches an agent's permission ruleset
//     - s10 (tool-loop) — calls Evaluate before every tool dispatch
//     - s12 (mcp-client) — gates MCP-loaded tools the same way
//   The shape of evaluate.ts is the contract every consumer relies on:
//     (permission, target, ...rulesets) → "allow" | "deny" | "ask".
//
// What we rebuilt in Go (s04):
//   - `evaluate(permission, pattern, ...rulesets)` → `Evaluate(permission, target, rulesets...) Action`
//   - `Rule = { permission, pattern, action: "allow"|"deny"|"ask" }` → `Rule struct { Permission, Pattern string; Action Action }`
//   - the `findLast` semantics — same algorithm, same fallback to "ask"
//   - `Wildcard.match` — same regex strategy (* → .*, ? → ., trailing " *" → "( .*)?")
//
// What we DID NOT rebuild yet (lives in later sessions):
//   - the surrounding `index.ts` Service that owns the in-memory request/reply
//     queue (live "ask" prompts) — that's s09/s10 territory
//   - drizzle-orm persistence of approved rules (PermissionTable, Approval) — s07/s09
//   - the structured `allStructured` matcher for "shell head + tail" rules — s10
//
// ---- begin upstream excerpt: packages/opencode/src/permission/evaluate.ts (full file) ----

import { Wildcard } from "@/util/wildcard"

type Rule = {
  permission: string
  pattern: string
  action: "allow" | "deny" | "ask"
}

export function evaluate(permission: string, pattern: string, ...rulesets: Rule[][]): Rule {
  const rules = rulesets.flat()
  const match = rules.findLast(
    (rule) => Wildcard.match(permission, rule.permission) && Wildcard.match(pattern, rule.pattern),
  )
  return match ?? { action: "ask", permission, pattern: "*" }
}

// ---- end evaluate.ts (truly the whole file) ----
//
// The matcher that evaluate.ts leans on (util/wildcard.ts L3-L19):

export function match(str: string, pattern: string) {
  if (str) str = str.replaceAll("\\", "/")
  if (pattern) pattern = pattern.replaceAll("\\", "/")
  let escaped = pattern
    .replace(/[.+^${}()|[\]\\]/g, "\\$&") // escape special regex chars
    .replace(/\*/g, ".*") // * becomes .*
    .replace(/\?/g, ".") // ? becomes .

  // If pattern ends with " *" (space + wildcard), make the trailing part optional
  // This allows "ls *" to match both "ls" and "ls -la"
  if (escaped.endsWith(" .*")) {
    escaped = escaped.slice(0, -3) + "( .*)?"
  }

  const flags = process.platform === "win32" ? "si" : "s"
  return new RegExp("^" + escaped + "$", flags).test(str)
}

// ---- end wildcard.ts excerpt ----
//
// Reading map (in s04 order — later sessions read deeper):
//   1. evaluate.ts (whole file, 16 lines)        — the algorithm (this s04)
//   2. util/wildcard.ts L3-L19 (match)           — the matcher (this s04)
//   3. permission/index.ts L19-L30 (Schema)      — Rule / Action / Ruleset shapes
//   4. permission/index.ts L60-L200 (Service)    — request/reply queue (s09/s10)
//   5. config/permission.ts                      — how rules get loaded from opencode.json (s08)
//   6. session/processor.ts (where evaluate is called per tool dispatch) — (s10)
//
// The mental jump from upstream → s04 Go:
//   - `Array.findLast`                     → reverse-iterate or remember-last in Go (we do the latter)
//   - `Rule[][]` with `.flat()`            → variadic `...Ruleset` and a nested loop
//   - `Schema.Literals(["allow","deny","ask"])` → `Action` int enum, ActionAsk=0 as the safe default
//   - `Wildcard.match` regex compile       → `regexp.Compile` per call (cheap at s04's scale)
//   - JS string replaceAll                 → `strings.ReplaceAll`
// The verdict the consumer sees is identical: "allow" / "deny" / "ask".
