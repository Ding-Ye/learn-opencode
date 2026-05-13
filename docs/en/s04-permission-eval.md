---
title: "s04 · Permission evaluator"
chapter: 4
slug: s04-permission-eval
est_read_min: 9
---

# s04 · Permission evaluator

> What this teaches: the gate every tool dispatch has to walk through. s03 made the Registry callable; s04 makes "callable" not the same as "called." A 16-line algorithm — flatten rulesets, find the last match, return its Action — is enough to support the entire cascade pattern (defaults → user_config → agent_override) every later session leans on.

---

## Problem

s03 ended with `Registry.Dispatch(ctx, name, input)` happily running any tool the LLM names. That's a problem the moment we register `bash`: the LLM can — and will, given the wrong prompt — emit `tool_use{name: "bash", input: {cmd: "rm -rf /"}}`, and our Registry has no way to say no.

We need a verdict function. Given a permission name (`edit`, `bash`, `webfetch`, …) and a target (a file path, a command line, a URL — whatever the specific permission domain operates on), it returns one of three answers: **allow** (run it), **deny** (refuse), **ask** (surface a prompt to the human and wait).

opencode's `packages/opencode/src/permission/evaluate.ts` is 16 lines — the entire file. The brevity is load-bearing: the surrounding `index.ts` (live request/reply queues, drizzle persistence, websocket bus broadcasts) is hundreds of lines, but the *decision* is 16. If we can't isolate that, every later session has to reimplement the cascade. So s04 isolates exactly that.

## Solution

`Evaluate(permission, target string, rulesets ...Ruleset) Action`. The whole module is ~150 LOC, no dependencies, no I/O, no goroutines.

The algorithm:

1. Walk every Rule in every Ruleset, in argument order. Rulesets are passed in cascade order: built-in defaults first, user opencode.json next, agent override last.
2. For each Rule, check both `Rule.Permission == permission` (wildcard-matched, so `*` rules work) AND `wildcardMatch(target, Rule.Pattern)`.
3. Remember the LAST one that matched. (Last-match-wins is what makes the cascade compose — a later layer can always tighten or loosen an earlier one.)
4. Return its Action. If nothing matched, return `ActionAsk` — the safe default, also the zero value of the enum so an uninitialized `Rule{}` already means "ask."

The wildcard matcher is a Go port of upstream's `util/wildcard.ts`: regex-based, with `*` → `.*`, `?` → `.`, plus the trailing-` *` → `( .*)?` special case so `git diff *` matches a bare `git diff`.

## How It Works

```
┌──────────────────────────────────────────────────────────────────┐
│  s04 Permission evaluator                                        │
│                                                                  │
│   Evaluate("edit", "main.go", defaults, userConfig, agentOverride)│
│                                                                  │
│   defaults:        [{edit, *,           ASK }]                   │
│   userConfig:      [{edit, *.go,        ALLOW}]                  │
│   agentOverride:   [{edit, secrets.go,  DENY}]                   │
│                                                                  │
│   Walk in order, remembering the LAST match:                     │
│     defaults[0]:        permission match, target match  → ASK    │
│     userConfig[0]:      permission match, target match  → ALLOW  │
│     agentOverride[0]:   permission match, target NO     → skip   │
│                                                                  │
│   Last match: ALLOW.  Return ActionAllow.                        │
│                                                                  │
│   ────────────────────────────────────────────────────           │
│                                                                  │
│   Evaluate("edit", "secrets.go", defaults, userConfig, agentOverride)│
│     defaults[0]:        match → ASK                              │
│     userConfig[0]:      match → ALLOW                            │
│     agentOverride[0]:   match → DENY                             │
│                                                                  │
│   Last match: DENY.  Return ActionDeny.                          │
└──────────────────────────────────────────────────────────────────┘
```

The signature in `permission.go`:

```go
type Action int

const (
    ActionAsk   Action = iota // zero value: safe default
    ActionAllow
    ActionDeny
)

type Rule struct {
    Permission string
    Pattern    string
    Action     Action
}

type Ruleset []Rule

func Evaluate(permission, target string, rulesets ...Ruleset) Action {
    var matched *Rule
    for ri := range rulesets {
        rs := rulesets[ri]
        for i := range rs {
            r := &rs[i]
            if !wildcardMatch(permission, r.Permission) { continue }
            if !wildcardMatch(target, r.Pattern)        { continue }
            matched = r
        }
    }
    if matched == nil { return ActionAsk }
    return matched.Action
}
```

**Three non-obvious points**:

1. **Last match wins, not first.** Upstream uses `Array.findLast`. The whole reason is the cascade: a built-in default of "ask everything" gets overridden by the user's `allow *.go`, which gets overridden by an agent's `deny secrets.go`. If we returned the first match, every layer would have to know about every other layer — and the user couldn't loosen a default without rewriting it.
2. **`ActionAsk` is the zero value.** A bare `Rule{}` is "ask"; a no-match outcome is "ask"; an empty ruleset is "ask". The safest default is the simplest one to construct accidentally — that's the Go-idiomatic way to fail closed.
3. **Wildcard is regex-based, not `path.Match`.** Go's stdlib `path.Match` doesn't let `*` cross a `/` boundary, which would silently break a rule like `src/*.go` matching `src/lib/foo.go`. Upstream compiles a regex from the pattern with `*` → `.*` and `?` → `.`, and we do the same — same byte-level match behavior across both runtimes.

## What Changed (vs. s03)

s03's `Registry.Dispatch` runs any tool by name unconditionally. There's no place in the call site to say "the LLM is allowed to call `read` but not `bash`." s04 supplies the verdict function s10's loop will wrap around dispatch:

```diff
 // s03: dispatch is unconditional.
 result, err := reg.Dispatch(ctx, name, input)

+// s04: dispatch is gated by a permission verdict.
+verdict := Evaluate(toolToPermission(name), targetFromInput(input), rulesets...)
+switch verdict {
+case ActionAllow:
+    result, err := reg.Dispatch(ctx, name, input)
+case ActionDeny:
+    return synthErrorResult("denied by permission rule")
+case ActionAsk:
+    // s10 will surface a Question Part; s04 doesn't reach into the loop.
+}
```

s04 stays a pure function — no goroutines, no UI, no I/O. The integration with the live "ask the human" prompt (a websocket-broadcast Question Part with a deferred reply) lives in s09 (agent-registry) and s10 (tool-loop). That separation is what lets the same `Evaluate` serve every consumer (loop, MCP, LSP) without each one re-deriving the cascade rule.

## Try It

```bash
cd agents/s04-permission-eval

# Demo: 3-layer cascade × 7 probes; each verdict printed inline.
go run .

# 5 tests, no network, no I/O, no clock dependency.
go test -count=1 ./...

# Confirm the demo cascade produces the expected verdicts:
go run . | grep -c '→ allow'   # 2 (main.go + git status)
go run . | grep -c '→ deny'    # 2 (secrets.go + rm -rf /)
go run . | grep -c '→ ask'     # 3 (README.md + echo hi + webfetch)
```

## Upstream Source Reading

The mechanism this s04 mirrors is opencode's `packages/opencode/src/permission/evaluate.ts` — which, remarkably, is the entire file. The surrounding `index.ts` Service that owns the live request/reply queue is much larger, but the verdict logic is exactly these 16 lines. We reproduce the whole file here because there's nothing to omit:

```ts
// upstream:packages/opencode/src/permission/evaluate.ts (whole file, L1-L16)
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
```

And the matcher `evaluate.ts` calls — `util/wildcard.ts` lines 3–19, the function whose semantics we have to preserve byte-for-byte:

```ts
// upstream:packages/opencode/src/util/wildcard.ts#L3-L19
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
```

Permalinks:

- evaluate.ts (whole file): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/permission/evaluate.ts#L1-L16>
- wildcard.ts match(): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/util/wildcard.ts#L3-L19>

What we kept and what we dropped:

- **Kept** — the Rule shape, the `findLast` semantics, the fallback to `{action: "ask"}`, the wildcard matcher's regex strategy with the trailing-` *` ergonomics. The verdict the caller sees is identical.
- **Dropped (for now)** — the surrounding `permission/index.ts` Service (live request/reply queues, websocket broadcast of "ask" prompts, drizzle-orm persistence of approvals); the `allStructured` matcher for shell-head-plus-tail patterns (s10 territory once we model parsed bash); per-platform regex flags (`"si"` on Windows). These are integration concerns, not algorithm concerns.
- **Forward-compat** — the live "ask the human" loop will sit *around* `Evaluate` in s09/s10, calling it as a pure function and translating ActionAsk into a Question Part with a deferred reply.

Reading order for opencode's permission layer:

1. `packages/opencode/src/permission/evaluate.ts` lines 1–16 — the algorithm (this s04)
2. `packages/opencode/src/util/wildcard.ts` lines 3–19 — the matcher (this s04)
3. `packages/opencode/src/permission/index.ts` lines 19–30 — the Schema declarations of Rule / Action / Ruleset
4. `packages/opencode/src/permission/index.ts` lines 60–200 — the live request/reply Service (s09 + s10)
5. `packages/opencode/src/config/permission.ts` — how rules get loaded from opencode.json (s08)
6. `packages/opencode/src/session/processor.ts` — where `evaluate` is called per tool dispatch (s10)
