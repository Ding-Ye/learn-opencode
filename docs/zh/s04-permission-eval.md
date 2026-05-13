---
title: "s04 · 权限求值"
chapter: 4
slug: s04-permission-eval
est_read_min: 9
---

# s04 · 权限求值

> 本章教什么：每次工具 dispatch 都得过的那道闸。s03 让 Registry 可以被调用；s04 让"可以被调用"不等于"被调用"。一段 16 行的算法 —— 把所有 ruleset 拍平、找最后一个 match、返回它的 Action —— 就足以撑起后面每节都要用到的整套 cascade 模式（defaults → user_config → agent_override）。

---

## Problem

s03 收尾时 `Registry.Dispatch(ctx, name, input)` 已经能愉快地跑 LLM 命名的任何工具。这就是问题：一旦我们注册 `bash`，LLM 完全可能 —— 给个错的 prompt 它就会 —— 发出 `tool_use{name: "bash", input: {cmd: "rm -rf /"}}`，而我们的 Registry 没有任何机制说 no。

我们需要一个判定函数。给一个 permission 名（`edit`、`bash`、`webfetch`、……）和一个 target（一个文件路径、一条命令、一个 URL —— 看具体 permission 域操作什么），返回三种答案之一：**allow**（跑）、**deny**（拒）、**ask**（向人类弹 prompt 等回复）。

opencode 的 `packages/opencode/src/permission/evaluate.ts` 一共 16 行 —— 整个文件。这种短是 *load-bearing* 的：周围的 `index.ts`（实时 request/reply 队列、drizzle 持久化、websocket bus 广播）几百行，但 *决策* 就这 16 行。如果我们没法把它隔离出来，后面每节都要重新实现一次 cascade。所以 s04 就专门把这一段隔离出来。

## Solution

`Evaluate(permission, target string, rulesets ...Ruleset) Action`。整个模块约 150 LOC，无依赖、无 I/O、无 goroutine。

算法：

1. 按 argument 顺序遍历每个 Ruleset 里的每个 Rule。Ruleset 按 cascade 顺序传入：内置 defaults 在前、用户 opencode.json 居中、agent override 最后。
2. 每个 Rule 检查两件事：`Rule.Permission == permission`（用 wildcard 匹配，所以 `*` 规则也工作）以及 `wildcardMatch(target, Rule.Pattern)`。
3. 记住 *最后* 一个匹配上的。（last-match-wins 是 cascade 能 compose 的根本 —— 后一层永远能收紧或放开前一层。）
4. 返回它的 Action。如果什么都没匹配上，返回 `ActionAsk` —— 安全默认，也正好是 enum 的零值，所以一个未初始化的 `Rule{}` 已经表示"ask"。

wildcard matcher 是上游 `util/wildcard.ts` 的 Go port：基于 regex，`*` → `.*`、`?` → `.`，加上结尾 ` *` → `( .*)?` 这个特殊处理 —— 这样 `git diff *` 能匹配光秃秃的 `git diff`。

## How It Works

```
┌──────────────────────────────────────────────────────────────────┐
│  s04 权限求值                                                     │
│                                                                  │
│   Evaluate("edit", "main.go", defaults, userConfig, agentOverride)│
│                                                                  │
│   defaults:        [{edit, *,           ASK }]                   │
│   userConfig:      [{edit, *.go,        ALLOW}]                  │
│   agentOverride:   [{edit, secrets.go,  DENY}]                   │
│                                                                  │
│   按顺序走，记住 *最后* 一个 match：                                │
│     defaults[0]:        permission 中, target 中  → ASK          │
│     userConfig[0]:      permission 中, target 中  → ALLOW        │
│     agentOverride[0]:   permission 中, target 不中 → 跳过         │
│                                                                  │
│   最后匹配：ALLOW。返回 ActionAllow。                              │
│                                                                  │
│   ────────────────────────────────────────────────────           │
│                                                                  │
│   Evaluate("edit", "secrets.go", defaults, userConfig, agentOverride)│
│     defaults[0]:        match → ASK                              │
│     userConfig[0]:      match → ALLOW                            │
│     agentOverride[0]:   match → DENY                             │
│                                                                  │
│   最后匹配：DENY。返回 ActionDeny。                                │
└──────────────────────────────────────────────────────────────────┘
```

`permission.go` 里的签名：

```go
type Action int

const (
    ActionAsk   Action = iota // 零值：安全默认
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

**三个不显而易见的点**：

1. **Last match wins，不是 first。** 上游用 `Array.findLast`。理由就是 cascade：内置默认 "ask everything"，被用户的 `allow *.go` 覆盖，又被 agent 的 `deny secrets.go` 覆盖。如果返回第一个 match，每一层都得知道其他每一层 —— 而且用户没法在不重写默认规则的前提下放开某条限制。
2. **`ActionAsk` 是零值。** 一个 bare `Rule{}` 是 "ask"；no-match 的结果是 "ask"；空 ruleset 是 "ask"。最安全的默认就是最容易被意外构造出来的那个 —— 这是 Go idiomatic 的 fail-closed 写法。
3. **Wildcard 是 regex-based 的，不是 `path.Match`。** Go 标准库的 `path.Match` 不让 `*` 跨过 `/`，那样 `src/*.go` 这种规则就会悄悄不能匹配 `src/lib/foo.go`。上游是把 pattern 编译成 regex（`*` → `.*`、`?` → `.`），我们也一样 —— 两边运行时的 byte-level 匹配行为一致。

## What Changed (vs. s03)

s03 的 `Registry.Dispatch` 无条件地按名跑工具。调用点没有任何位置可以表达「LLM 可以调 `read` 但不能调 `bash`」。s04 提供 s10 loop 会缠在 dispatch 外面的那个判定函数：

```diff
 // s03: dispatch 是无条件的。
 result, err := reg.Dispatch(ctx, name, input)

+// s04: dispatch 由权限判定门把守。
+verdict := Evaluate(toolToPermission(name), targetFromInput(input), rulesets...)
+switch verdict {
+case ActionAllow:
+    result, err := reg.Dispatch(ctx, name, input)
+case ActionDeny:
+    return synthErrorResult("denied by permission rule")
+case ActionAsk:
+    // s10 会发一个 Question Part；s04 不伸进 loop 里。
+}
```

s04 保持纯函数 —— 无 goroutine、无 UI、无 I/O。和实时「问人类」prompt 的集成（websocket-broadcast 一个 Question Part，然后 deferred reply）落在 s09（agent-registry）和 s10（tool-loop）。这种切分让同一个 `Evaluate` 能服务每一种 consumer（loop、MCP、LSP）而每个 consumer 都不必自己重新推一遍 cascade 规则。

## Try It

```bash
cd agents/s04-permission-eval

# Demo：3 层 cascade × 7 个 probe；每个判定就地打印。
go run .

# 5 个测试，无网络、无 I/O、无时钟依赖。
go test -count=1 ./...

# 检查 demo cascade 产出的判定数量是否符合预期：
go run . | grep -c '→ allow'   # 2 (main.go + git status)
go run . | grep -c '→ deny'    # 2 (secrets.go + rm -rf /)
go run . | grep -c '→ ask'     # 3 (README.md + echo hi + webfetch)
```

## Upstream Source Reading

s04 镜像的机制是 opencode 的 `packages/opencode/src/permission/evaluate.ts` —— 神奇的是这就是整个文件。周围的 `index.ts` Service（管实时 request/reply 队列）大得多，但判定逻辑就这 16 行。我们这里把整个文件抄下来，因为没有什么可以省略：

```ts
// upstream:packages/opencode/src/permission/evaluate.ts (整个文件, L1-L16)
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

以及 `evaluate.ts` 调用的那个 matcher —— `util/wildcard.ts` 3–19 行，我们必须按字节保留语义的那个函数：

```ts
// upstream:packages/opencode/src/util/wildcard.ts#L3-L19
export function match(str: string, pattern: string) {
  if (str) str = str.replaceAll("\\", "/")
  if (pattern) pattern = pattern.replaceAll("\\", "/")
  let escaped = pattern
    .replace(/[.+^${}()|[\]\\]/g, "\\$&") // 转义 regex 特殊字符
    .replace(/\*/g, ".*") // * 变 .*
    .replace(/\?/g, ".") // ? 变 .

  // 如果 pattern 以 " *"（空格+通配）结尾，把尾段变成可选
  // 这样 "ls *" 既能匹配 "ls" 也能匹配 "ls -la"
  if (escaped.endsWith(" .*")) {
    escaped = escaped.slice(0, -3) + "( .*)?"
  }

  const flags = process.platform === "win32" ? "si" : "s"
  return new RegExp("^" + escaped + "$", flags).test(str)
}
```

Permalink：

- evaluate.ts（整个文件）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/permission/evaluate.ts#L1-L16>
- wildcard.ts match()：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/util/wildcard.ts#L3-L19>

我们留下了什么、砍了什么：

- **留下** —— Rule 形状、`findLast` 语义、回落到 `{action: "ask"}`、wildcard matcher 的 regex 策略以及结尾 ` *` 的人体工程学。调用方看到的判定字节级一致。
- **暂时砍掉** —— 周围的 `permission/index.ts` Service（实时 request/reply 队列、"ask" prompt 的 websocket 广播、approval 的 drizzle-orm 持久化）；`allStructured` 那个 shell-head-plus-tail 的 matcher（s10 解析 bash 之后再说）；按平台不同的 regex flag（Windows 用 `"si"`）。这些是集成关注点，不是算法关注点。
- **向前兼容** —— 实时「问人类」的 loop 会在 s09/s10 把 `Evaluate` 包在外面，把它当纯函数调用，再把 ActionAsk 翻译成带 deferred reply 的 Question Part。

opencode 权限层的阅读顺序：

1. `packages/opencode/src/permission/evaluate.ts` 1–16 行 —— 算法本身（本节 s04）
2. `packages/opencode/src/util/wildcard.ts` 3–19 行 —— matcher（本节 s04）
3. `packages/opencode/src/permission/index.ts` 19–30 行 —— Rule / Action / Ruleset 的 Schema 声明
4. `packages/opencode/src/permission/index.ts` 60–200 行 —— 实时 request/reply Service（s09 + s10）
5. `packages/opencode/src/config/permission.ts` —— 规则怎么从 opencode.json 加载（s08）
6. `packages/opencode/src/session/processor.ts` —— 每次工具 dispatch 时 `evaluate` 在哪儿被调（s10）
