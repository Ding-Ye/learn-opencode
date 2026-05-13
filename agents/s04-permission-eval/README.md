# s04 — permission-eval

s03 gave us a Registry that can dispatch any tool the LLM names. That's a problem: the LLM can name `bash` with input `rm -rf /` and our Registry will happily run it. s04 inserts the gate: a tiny ruleset evaluator that, given a permission name (`edit`, `bash`, `webfetch`, …) and a target (a file path, a command line), returns `allow` / `deny` / `ask` — and any later session that wraps tool dispatch must consult it first.

The mechanism is intentionally small: ~150 LOC, no dependencies, no I/O. The only interesting choice is **last-match-wins** semantics across multiple rulesets — that's what makes the cascade pattern (`defaults → user_config → agent_override`) compose naturally without rewriting earlier layers.

## Files

- `permission.go` — the whole module:
  - `Action` enum (`ActionAsk` zero-value default, `ActionAllow`, `ActionDeny`) with a `String()` for printable test failures.
  - `Rule struct { Permission, Pattern string; Action Action }` and `Ruleset []Rule`.
  - `Evaluate(permission, target string, rulesets ...Ruleset) Action` — flattens rulesets in argument order, finds the LAST rule where `Permission` and `Pattern` both match, returns its Action; falls through to `ActionAsk` when nothing matches.
  - `wildcardMatch(s, pattern)` — Go port of opencode's `util/wildcard.ts`: regex-based, `*` → `.*`, `?` → `.`, with the trailing-` *` → `( .*)?` special case so `git diff *` matches a bare `git diff`.
- `main.go` — runnable demo: builds a 3-layer cascade and queries it for 7 (permission, target) probes, printing the verdict for each.
- `permission_test.go` — 5 tests:
  1. **empty rulesets → ask** — the safe default.
  2. **single allow rule** — happy path; also confirms permission-name mismatch falls through to ask.
  3. **later deny overrides earlier allow** — the load-bearing last-match-wins assertion.
  4. **pattern glob** — `*.go` matches `main.go` not `main.py`; `*` matches the empty string.
  5. **multi-ruleset cascade** — defaults + user + agent; the agent's deny on a specific path overrides the user's broad allow.

## Run

```
go run .                 # demo: 7 probes against a 3-layer cascade
go test -count=1 ./...   # 5 tests, no network, no I/O
```

## What this maps to upstream

| This file              | Upstream file                                                                           |
|------------------------|------------------------------------------------------------------------------------------|
| `permission.go` `Evaluate` | `packages/opencode/src/permission/evaluate.ts` (the whole 16-line file)              |
| `permission.go` `wildcardMatch` | `packages/opencode/src/util/wildcard.ts` `match()` (lines 3–19)                  |
| `permission.go` `Rule` / `Action` | `packages/opencode/src/permission/index.ts` `Rule` / `Action` (lines 19–30)    |

## Key teaching points

- **Last match wins, NOT first.** Upstream uses `Array.findLast`. The whole reason is the cascade: a built-in default of "ask everything" gets overridden by the user's `allow *.go`, which gets overridden by an agent's `deny secrets.go`. If we returned the first match, every layer would have to know about every other layer — and the user couldn't loosen a default without rewriting it.
- **`ActionAsk` is the zero value.** A bare `Rule{}` is "ask"; a no-match outcome is "ask"; an empty ruleset is "ask". The safest default is the simplest one to construct accidentally — that's the Go-idiomatic way to fail closed.
- **The Pattern field is the target glob, not the `permission:pattern` shape.** Upstream's user-facing config writes `"edit:*.go"` as a single key, but `evaluate(permission, pattern, ...)` already splits them. We follow that internal split: the caller passes the bare permission name, the rule stores just the target glob. The `:` parsing happens at config-load time (s08), not here.
- **Wildcard matcher is regex-based, not `path.Match`.** Go's `path.Match` doesn't let `*` match `/`, which would break a rule like `src/*.go` matching `src/lib/foo.go`. opencode's matcher is a regex compile of `pattern.replace('*', '.*')` — same as what we do, with the `(?s)` flag so `.` matches newlines (heredoc-style tool inputs).
- **The trailing-` *` special case is shell-ergonomic.** `git diff *` matching `git diff` (no args) is what lets users write a single permission rule for a command family without two entries. It's a 3-line trick in upstream and a 3-line trick here.

## What changed vs s03

s03's `Registry.Dispatch` runs any tool by name unconditionally — there's no place to say "the LLM is allowed to call `read` but not `bash`." s04 adds the gate that s10's loop will wrap around dispatch:

```diff
 // s03: dispatch is unconditional.
 result, err := reg.Dispatch(ctx, name, input)

+// s04: dispatch is gated by a permission verdict.
+verdict := permission.Evaluate(toolToPermission(name), targetFromInput(input), rulesets...)
+switch verdict {
+case ActionAllow:
+    result, err := reg.Dispatch(ctx, name, input)
+case ActionDeny:
+    return errors.New("denied by permission rule")
+case ActionAsk:
+    // s10 will surface a Question Part; for now, treat as deny.
+}
```

s10 wires this in for real — it's where the verdict turns into either a `tool_result` or a `Question` Part. s04 just supplies the `Evaluate` function; the integration is its consumer's problem.

See `docs/zh/s04-permission-eval.md` and `docs/en/s04-permission-eval.md` for the long-form walkthrough plus the upstream excerpt from `packages/opencode/src/permission/evaluate.ts`.
