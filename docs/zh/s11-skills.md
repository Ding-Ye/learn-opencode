---
title: "s11 · 技能发现"
chapter: 11
slug: s11-skills
est_read_min: 10
---

# s11 · 技能发现

> 本章教什么：扫描一组 skill 根目录，找到每一个一层深度的 `SKILL.md`，把 YAML frontmatter 解成 `(name, description, when_to_use, body)`，按 name 去重并 **last-wins**，再渲染成系统提示词可直接拼接的 catalog 字符串。这条机制让用户只要 `mkdir -p .opencode/skills/git-helper && $EDITOR SKILL.md`，下一次会话的 LLM 就能知道「用户提到 git 的时候用 git-helper 这个 skill」—— 不改代码，不重启，只编辑 SKILL.md。

---

## Problem

到 s10 为止 agent 的 tool loop 已经能跑了，但每次 prompt 只有 (system prompt) + (user message) + (tool catalog)。用户想要第三样东西：**skills** —— 用户在磁盘上丢一个 `SKILL.md` 说「这是个食谱，X 情况下用」。skill 的 frontmatter 写 (name, description, 触发提示)，body 是模型挑了这个 skill 之后才读的食谱内容。

没有 skill 的 agent 真实痛点：

- 用户有一个个人「conventional-commits 加 emoji」的 commit 食谱。他**不**想把它塞进每次对话的 system prompt —— 他只想 commit 的时候模型才用这个食谱。
- 同一个项目里既有「code-search 用 ripgrep + `--type-not vendor`」的习惯，又有「部署走 make-deploy 脚本」的习惯。两个独立 skill，两个不同触发，模型都该能选。
- 用户 `~/.opencode/skills/git-helper/SKILL.md` 有全局 skill，又在 `.opencode/skills/git-helper/SKILL.md` 有项目级覆写。项目覆写应该赢 —— 用户期望更具体的版本胜出。

opencode 的解法是 **从一组 skill 根目录扫 SKILL.md，每个 frontmatter 解析、按 name 去重 last-wins、把 catalog 注入 system prompt**。每个 SKILL.md 的 body 不是每个请求都读 —— 它在那里等模型选了 skill 之后再读（一个未来的 `read_skill` 工具来 LLM 调用）。

s11 搭的是 *discover + parse + catalog* 流水线。它**不**做：

- 把 `(agent system) + (skill catalog) + ...` 拼成 system prompt 的那一段（s10 做，后续 session 扩展）。
- 模型选了 skill 之后取 body 的 `read_skill` 工具（out of scope；30 行 `os.ReadFile` 包装）。
- 从 git URL 拉的 `cfg.skills.urls` 那一类 discovery（out of scope；要 git 客户端）。
- 「外部目录」discovery（`.claude/`, `.agents/`）—— 同样的机制，只是不同根目录；cfg-paths + s11 已经覆盖。

## Solution

一个 struct，三个函数，一个解析器：

```go
type Skill struct {
    Name        string `yaml:"name"`
    Description string `yaml:"description"`
    WhenToUse   string `yaml:"when_to_use"`
    Body        string `yaml:"-"`
    Path        string `yaml:"-"`
}

func ParseSkillMD(path string, content []byte) (*Skill, error)
func DiscoverSkills(dirs []string) ([]*Skill, error)
func CatalogString(skills []*Skill) string
```

各自做什么：

- **`ParseSkillMD`**：在第一行的 `---` 和下一个 `---` 行之间切。把 frontmatter `yaml.Unmarshal` 进 struct 的三个带 yaml tag 的导出字段。Body 是结尾 `---` 之后的所有内容（开头空行 trim）。缺开头 delimiter、缺结尾 delimiter、缺必填 `name` 都是 hard error，错误消息里带文件路径。
- **`DiscoverSkills`**：对 `dirs` 里每个目录，列出**直接**子目录（一层深度），每个子目录里看有没有 `SKILL.md`。所有找到的 SKILL.md 都解析。按 `name` 去重 **last-wins**（`dirs` 里靠后的覆盖靠前的）。根目录不存在静默跳过（mirror 上游 `if (!isDir(root)) continue`）；任何文件解析出错就让整个调用失败（教学契约）。
- **`CatalogString`**：按输入 slice 的顺序，每个 skill 渲染一行。格式：`- <name>: <description> (use when: <when_to_use>)`。`WhenToUse` 为空 → 省掉后缀。空输入 → 空字符串（这样调用方可以 `if cat != "" { ... }` 来 gate 整段「Available Skills:」prompt）。

**为什么一层深度，不递归**：上游 glob 是 `"skills/**/SKILL.md"`（或 URL-pulled 那种 `"**/SKILL.md"`），技术上 skill 可以在任意深度。实际上上游所有 skill 都正好在一层 —— `skills/<name>/SKILL.md`。限定一层让 discovery 契约一目了然，测试面也小。要上游行为的话 `DiscoverSkillsRecursive` 一个 `filepath.Walk` 就能加。

**为什么重名 last-wins，不是 first-wins**：mirror 上游 L116-L122。理由：调用方按优先级传 `dirs`，**最高优先级排最后**。项目本地 skill（最具体）排在 user-home（不那么具体）后面。项目里 ship 一个 `git-helper` 覆盖全局的，项目版本就是用户想要的 —— last-wins 让它成为默认。

**为什么解析错误 fail-loud，不静默跳过**：上游 `add()` L114 看到 frontmatter 不对就静默 return（L101-L106 log warning 然后忽略文件）。教学仓库这个 trade-off 不对 —— 跑 `go test` 的人不该自己想为什么 SKILL.md 被忽略了。我们返回带文件名的 error。生产 opencode 是 log-and-continue，我们换更清晰的失败模式。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s11 skill discovery 流水线                                            │
│                                                                        │
│   dirs = ["~/.opencode/skills", ".opencode/skills"]                    │
│           ↑ 低优先级                  ↑ 高优先级（排最后）             │
│                                                                        │
│   DiscoverSkills(dirs)                                                 │
│     for root in dirs:                                                  │
│       for sub in os.ReadDir(root):       ← 一层深度                    │
│         if exists(<root>/<sub>/SKILL.md):                              │
│           skill = ParseSkillMD(...)                                    │
│           byName[skill.Name] = skill     ← 重名 LAST-WINS              │
│                                                                        │
│   ParseSkillMD(path, content)                                          │
│     1. content[0:3] 必须是 "---" 行       (否则 error)                 │
│     2. 找下一个 "---" 行                  (否则 error)                 │
│     3. yaml.Unmarshal(frontmatter, &Skill)                             │
│     4. 要求 Name 非空                     (否则 error)                 │
│     5. Body = 结尾 delim 之后的内容，开头换行 trim                     │
│                                                                        │
│   CatalogString(skills) → string                                       │
│     for s in skills:                                                   │
│       "- <s.Name>: <s.Description> (use when: <s.WhenToUse>)"          │
│     WhenToUse 空 → 省略 "(use when: ...)" 后缀                         │
│     输入空 → 返回 ""                                                   │
│                                                                        │
│   ┌──────────────────────────────────────┐                             │
│   │  s10 的 loop（后续）会预拼：         │                             │
│   │    cat = CatalogString(skills)       │                             │
│   │    if cat != "":                     │                             │
│   │      systemPrompt += "\n## Available Skills\n" + cat               │
│   └──────────────────────────────────────┘                             │
└────────────────────────────────────────────────────────────────────────┘
```

**四个 load-bearing 决定**：

1. **一层深度**。`<root>/<skill-name>/SKILL.md`。不是零层（没有 `<root>/SKILL.md`），不是两层（没有 `<root>/<a>/<b>/SKILL.md`）。把测试面收成单一形状；匹配上游所有真实 skill；要的话 `filepath.Walk` 就能扩。
2. **跨 dir 重名 last-wins**。调用方按优先级排好 `dirs`，最低排最前。项目覆写全局，agent-specific 覆写项目，URL-pulled 覆写所有（上游布局）。单个 dir 内部按子目录名排序，所以顺序确定。
3. **解析错误 fail-loud**。SKILL.md 没有 `---` 或没 `name` 返回包带文件路径的 error。上游 log + skip；我们换更清晰的测试信号。（要 log-and-continue 的话外面套个 `errors.Is` 就行。）
4. **catalog 格式是结构性的，不是策略**。`CatalogString` 不排序、不按 agent permission 过滤、不去重 —— 它只渲染。调用方（未来 s10 的 loop）做策略 —— 调用前自己 sort.Slice、调用前用 `Permission.evaluate("skill", name, agent.permission)` 过滤等。格式跟策略分开。

**为什么 ~350 LOC（含测试）**：因为活儿小。Skill struct 5 个字段。ParseSkillMD 一个 line-scan + `yaml.Unmarshal`。DiscoverSkills 一个 `os.ReadDir` + 循环。CatalogString 一个 `strings.Builder`。5 个测试覆盖每条路径。没有 I/O 编排，没有 Effect 包装，没有 Service/Layer 管道 —— Go 标准库 + `gopkg.in/yaml.v3` 够用。

## What Changed (vs. s08)

s08 把 `opencode.json` 加载成 `Config` struct。s11 拿 Config 里的某一个字段（用户配置的 skill 路径），把它变成一段 system prompt 片段。Config 管道一行没改；新加的层纯粹是「拿到 Config 解出来的 skill dirs，扫盘，产 catalog」。

```diff
 // s08: Config 暴露用户配置的 skill dirs（和其它路径）。
 cfg, _ := Load(cwd, homeDir, EnvFromOS())
 // cfg.SkillPaths 是 SKILL.md 扫描根目录的 []string。

+// s11: 扫这些 dirs，拼 catalog，拼到 system prompt 里。
+skills, err := DiscoverSkills(cfg.SkillPaths)
+if err != nil {
+    return fmt.Errorf("skill discovery: %w", err)
+}
+catalog := CatalogString(skills)
+if catalog != "" {
+    systemPrompt += "\n\n## Available Skills\n" + catalog
+}
+// systemPrompt 接着喂给 s10 loop 的第一次请求。
```

`Config` 形状一行没改 —— s08 的「Config 是 dumb data」决定继续兑现。s11 添加的是 Config 某个字段的*消费者*。和 s09 给 `cfg.Permissions` 添加了 agent-cascade 消费者一样对称，都没动 Config struct。

s10 接下来要做的事：拼 system prompt 时把 `CatalogString(skills)` 预拼进去。模型看到一个结构化的 skill 菜单，可以决定「我应该用 git-helper」而不需要用户每次明确提及。模型挑到 skill 触发（未来的）`read_skill` 工具来读 body —— catalog 只承载够「让模型挑」的元信息。

## Try It

```bash
cd agents/s11-skills

# 演示（确定性，无网络）：
go run .

# 5 个测试：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

5 个测试覆盖：

1. **ParseSkillMDValid** —— `Skill` 每个字段都从正确来源填（frontmatter → Name/Description/WhenToUse，body content → Body，参数 → Path）。Body 不能含 frontmatter 残余。
2. **DiscoverSkillsOneLevelDeep** —— 找到 `<root>/<skill>/SKILL.md`；忽略 `<root>/SKILL.md`（零层）和 `<root>/<a>/<b>/SKILL.md`（两层）。钉住结构契约。
3. **DiscoverSkillsLastWinsOnDuplicateName** —— 两个 root 各 ship 一个 `git-helper` 但 description 不同，`dirs` 里靠后的赢。反转 `dirs` 也跟着翻。这是唯一钉住跨 dir 顺序语义的测试。
4. **ParseSkillMDMissingFrontmatter** —— 三个 sub-case（缺开头 `---`，缺结尾 `---`，缺 `name`）。每个返回的 error 消息里都带文件路径。
5. **CatalogStringFormat** —— 钉精确行格式 `- <name>: <description> (use when: <when_to_use>)`；验证三个 frontmatter 字段都出现在渲染输出里；验证 WhenToUse 为空时省略后缀；空输入返回空字符串。

## Upstream Source Reading

s11 mirror 的是 opencode 的 `packages/opencode/src/skill/index.ts`。整个文件 323 行，覆盖 Effect-Service/Layer 接线、外部 dir 扫、URL-pulled discovery、详尽 XML catalog 格式。s11 取的是 *核心机制*（L36-L161）：schema、带去重规则的 per-file `add()`、per-dir `scan()` glob。其它都是管道。

```ts
// upstream:packages/opencode/src/skill/index.ts L36-L161

// L36-L42 — 运行时 Info schema。我们 Go 这边 `Skill` struct 留这四个
// 字段（name, description, location → Path, content → Body），并
// 加一个 `WhenToUse` 让 catalog 渲结构化触发提示。上游则把触发塞到
// description 里。
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  location: Schema.String,
  content: Schema.String,
})

// L52-L58 — frontmatter 校验。只有 `name: string` 是必填；
// description 可选。我们 ParseSkillMD 也强制同样的契约。
function isSkillFrontmatter(data: unknown): data is { name: string; description?: string } {
  return (
    isRecord(data) &&
    typeof data.name === "string" &&
    (data.description === undefined || typeof data.description === "string")
  )
}

// L94-L131 — `add()`：解析一个 SKILL.md，挂到 state。
//   1. ConfigMarkdown.parse 切 frontmatter 和 body（我们在 ParseSkillMD
//      里手写同样的 `---` 扫描）。
//   2. L114 frontmatter 不合法就静默 skip（我们 fail-loud）。
//   3. L116-L122 重名 LAST-WINS（log warn，然后覆盖）。
//      我们 DiscoverSkills 一字不差地复刻。
const add = Effect.fnUntraced(function* (state: State, match: string, bus: Bus.Interface) {
  const md = yield* Effect.tryPromise({
    try: () => ConfigMarkdown.parse(match),
    catch: (err) => err,
  })
  if (!md) return
  if (!isSkillFrontmatter(md.data)) return                  // ← 上游这里静默 skip

  if (state.skills[md.data.name]) {
    log.warn("duplicate skill name", {                       // ★ last-wins:
      name: md.data.name,                                    //   warn 然后覆盖
      existing: state.skills[md.data.name].location,
      duplicate: match,
    })
  }

  state.dirs.add(path.dirname(match))
  state.skills[md.data.name] = {                             // ← 存 Info
    name: md.data.name,
    description: md.data.description,
    location: match,
    content: md.content,
  }
})

// L133-L161 — `scan()`: glob 单个 root 找 SKILL.md，把 match 累到
// ScanState。Pattern 是 `"skills/**/SKILL.md"`（深度 2+）或
// `"**/SKILL.md"`（任意）。我们 Go DiscoverSkills 限定深度 1
// （`<root>/<sub>/SKILL.md`），教学简化。
const scan = Effect.fnUntraced(function* (
  state: ScanState,
  root: string,
  pattern: string,
  opts?: { dot?: boolean; scope?: string },
) {
  const matches = yield* Effect.tryPromise({
    try: () =>
      Glob.scan(pattern, {
        cwd: root,
        absolute: true,
        include: "file",
        symlink: true,
        dot: opts?.dot,
      }),
    catch: (error) => error,
  })

  for (const match of matches) {                             // ★ 累每个 match
    state.matches.add(match)
    state.dirs.add(path.dirname(match))
  }
})
```

逐行注释（重点行）：

- **L36-L42 `Info` schema** —— 运行时 skill 的 *形状*。我们的 `Skill` struct 是它的超集（多了 `WhenToUse`）。`location` 字段是磁盘绝对路径，我们对应 `Path`。`content` 字段是 frontmatter 去掉之后的 markdown body，我们对应 `Body`。
- **L52-L58 `isSkillFrontmatter`** —— 唯一 HARD 必填是 `name: string`。我们 Go 这边 ParseSkillMD 同样强制：缺 `name` → error。Description 和我们扩展的 `when_to_use` 可选。
- **L114 `if (!isSkillFrontmatter(md.data)) return`** —— frontmatter 不合法静默 skip。我们 Go 这边返回 error。上游调用现场 catch + log（L98-L108），我们选浮出来让 `go test` 失败时自解释。
- **L116-L122 重名处理** —— 整章的钉子。state 已经有同名 skill → log warn，OVERWRITE。Last-wins。顺序由调用方决定（L173-L221 `discoverSkills`）：external dirs 先，config dirs 中间，URL-pulled dirs 最后。我们 Go `DiscoverSkills` 直接拿 dir 列表，调用方控顺序。
- **L125-L130 store** —— `state.skills[name] = { name, description, location, content }`。我们 Go `byName[s.Name] = s` 一样；byName map 是权威 state，`firstSeenOrder` slice 在覆盖时也保留遍历顺序，让返回的 slice 稳定。
- **L133-L161 scan glob** —— 真正的盘扫。上游用 Bun 的 `Glob.scan` + 上游 pattern；我们对每个 root 用 `os.ReadDir`，每个直接子目录里看有没有 `SKILL.md`。深度 1 的情况两边等价（这正好是所有真实 skill 的形状），Go 这边不用引依赖，简单很多。

permalink：

- Info schema (L36-L42): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L36-L42>
- isSkillFrontmatter (L52-L58): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L52-L58>
- add() per-file 解析 + last-wins (L94-L131): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L94-L131>
- scan() per-root glob (L133-L161): <https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L133-L161>

我们留下了什么，砍了什么：

- **留下** —— Info schema 的 load-bearing 字段（name, description, location, content）、`isSkillFrontmatter` 的 name-required 校验、重名 last-wins、per-root scan、深度 1 discovery（所有真实 skill 都在那）。
- **暂时砍掉** —— external-dir 扫（`.claude/`, `.agents/` 加 worktree up-walk，L173-L191）、URL-pulled discovery（L210-L215）、Effect-typed Service/Layer 接线（L232-L294）、Bus.publish 错误事件、内置 `customize-opencode` skill（L21-L34, L250-L257）、Permission 过滤的 `available(agent)`（L277-L282）、详尽 XML catalog 格式（L298-L313）。
- **向前兼容** —— `Skill` 加新字段（比如 `Version` 或 `License`）不破坏 parse 和 discovery。加 `DiscoverSkillsRecursive(dirs []string)` 做任意深度扫不动深度 1 契约。加 `Filter(skills, agent)` 做 permission cascade 过滤（s09 + s10 的活）是另一个函数 —— 不改 discovery 的输出。

opencode skill 层的阅读顺序：

1. `packages/opencode/src/skill/index.ts` L36-L42 —— `Info` Schema（s11 的 Skill struct 母本）
2. `packages/opencode/src/skill/index.ts` L94-L131 —— per-file `add()` 加 last-wins（s11 的 DiscoverSkills 核心）
3. `packages/opencode/src/skill/index.ts` L133-L161 —— per-root `scan()` glob（s11 的 DiscoverSkills 外层循环）
4. `packages/opencode/src/skill/index.ts` L296-L321 —— `fmt(list, opts)` catalog 渲染（s11 的 CatalogString 简化版）
5. `packages/opencode/src/config/markdown.ts` —— `ConfigMarkdown.parse` frontmatter 切分（s11 ParseSkillMD 手写等价物）
