---
title: "s07 · 会话存储"
chapter: 7
slug: s07-session-store
est_read_min: 12
---

# s07 · 会话存储

> 本章教什么：把 s02 / s06 一直在用的 `Message` of `Parts` *持久化*。三张 SQLite 表（sessions / messages / parts）+ 4 个 CRUD 函数 + adapter 函数把 Go struct 翻成 row 再翻回来。每一节后面（s10、s14）需要状态的地方，打开这个 DB 而不是在 RAM 里攒。SQLite 用 **`modernc.org/sqlite`** —— 纯 Go，无 CGo，CI 不需要 C toolchain。

---

## Problem

到 s06 为止，agent 的 `Message` 全部活在内存里。一次 `Loop.Consume` 跑完，得到一个 `*Message`，函数返回 —— 没了。要做实际有用的 agent，至少要解决：

- **跨进程恢复。** 用户关掉 CLI、第二天再来 —— 上一次的对话上下文必须还在。
- **多轮 tool loop 的中间态。** s10 的工具循环一次会跑 N 个 LLM 往返，每个往返产出新的 Message；如果中间崩了（rate limit、Ctrl-C），下次启动至少应该能看到「上次跑到哪儿」。
- **跨会话引用。** s09 的 sub-agent / parent-session 关系（一个会话 fork 出子会话）需要 `parent_id` 字段才有意义。
- **token / cost 累计。** s14 要把每次 Provider 调用的 Usage 累加到 session 行 —— 没行就没地方累加。

存在哪？opencode 选 SQLite —— 单文件、嵌入式、零运维。我们沿用，但故意 *不* 用 ORM（drizzle 在 TS 端做的事），手写 SQL；schema 故意保持小，从 23 列砍到 9 列，留出 token / cost 列让 s14 不需要 ALTER。

第二个限制：**不要 CGo**。`mattn/go-sqlite3` 的 CGo 版本快但 CI 要装 C toolchain；`modernc.org/sqlite` 是纯 Go transpiled SQLite，注册成 `"sqlite"` 驱动，`sql.Open("sqlite", ...)` 直接能跑。教学场景速度差异可以忽略。

## Solution

三张表 + 四个 func，加起来 ~500 LOC：

```go
type DB struct { /* wraps *sql.DB */ }

func Open(path string) (*DB, error)               // CREATE TABLE IF NOT EXISTS x3
func (db *DB) CreateSession(s *Session) error     // INSERT INTO sessions
func (db *DB) AppendMessage(sessionID string, m *Message) error  // tx: 1 message + N parts
func (db *DB) GetSession(id string) (*Session, []*Message, error)
func (db *DB) ListSessions(limit int) ([]*Session, error)
```

Schema：

| 表 | 主键 | 关键列 | 说明 |
|---|---|---|---|
| `sessions` | `id` | `slug`, `project_id`, `parent_id`, `created_at`, `updated_at`, `cost`, `input_tokens`, `output_tokens` | 一条会话 |
| `messages` | `id` | `session_id` (FK), `role`, `created_at` | 一条消息；`session_id` 外键到 sessions |
| `parts` | `id` | `message_id` (FK), `kind`, `payload`, `position` | 一个 Part；`payload` 是变体的 JSON，`kind` 是判别列 |

加上索引 `messages(session_id, created_at)` 和 `parts(message_id, position)` —— `GetSession` 的热路径。

**`Open` 的关键细节**：DSN 里加 `?_pragma=foreign_keys(1)`。SQLite 历史原因外键默认 OFF；不开的话测试 #5（向不存在的 session 插 message）会静默成功，孤儿行越攒越多没人发现。

**`AppendMessage` 包在事务里**。Message 行 + N 个 Part 行要么都进、要么都不进。中间失败留下「Message 存在但缺 Parts」是经典的悄悄数据腐蚀 —— 等到 s10 加载这个 session 再喂给 LLM 时，模型看到一个空 assistant 回复，行为难定位。

**`GetSession` 是 N+1 查询**：1 次 session + 1 次 messages + N 次 parts。可以折成一个 JOIN 然后 inflate，但 inflate 逻辑变难读。s07 不是性能章节，明确选 chatty-but-readable。

## How It Works

```
┌────────────────────────────────────────────────────────────────────────┐
│  s07 SQLite schema                                                      │
│                                                                        │
│   sessions ─┬─< messages ─┬─< parts                                     │
│             │             │                                            │
│   id (PK)   │   id (PK)   │   id (PK)                                  │
│   slug      │   session_id│   message_id (FK → messages.id)             │
│   project_id│   (FK → s.id)│  kind         ← discriminator column       │
│   parent_id │   role      │   payload      ← JSON of variant fields     │
│   created_at│   created_at│   position     ← sort key within message    │
│   updated_at│             │                                            │
│   cost      │             │                                            │
│   tokens_*  │             │                                            │
│                                                                        │
│   AppendMessage(sessionID, m):                                          │
│     tx.Begin()                                                          │
│     INSERT INTO messages (m.ID, sessionID, m.Role, m.CreatedAt)         │
│     for i, p := range m.Parts:                                          │
│       INSERT INTO parts (p.ID, m.ID, p.Kind, p.payloadJSON(), i)        │
│     tx.Commit()                                                         │
│                                                                        │
│   GetSession(id):                                                       │
│     SELECT * FROM sessions WHERE id = ?               → 1 row           │
│     SELECT * FROM messages WHERE session_id = ?                         │
│       ORDER BY created_at ASC, id ASC                 → N rows          │
│     for each msg:                                                       │
│       SELECT * FROM parts WHERE message_id = ?                          │
│         ORDER BY position ASC                          → M rows         │
└────────────────────────────────────────────────────────────────────────┘
```

**四个 load-bearing 设计**：

1. **`kind` 是它自己的列，`payload` 是 JSON。** 想找「session X 里所有 tool_use Part」的时候不需要 JSON 扫每行。和上游 `session.sql.ts` 同一思路（`text({ mode: "json" })` + 单独的 type 列在更外层 message-v2 里）。
2. **`created_at` / `position` 是 *load-bearing* 的排序列。** Message 按 `created_at ASC` 加载，Parts 按 `position ASC` 加载。Insert 顺序 *不能* 替代 —— 测试 #2 和 #3 专门写了「以反时序插入，断言读出来正序」来钉这一点。s10 的下一轮 LLM 调用要把历史 Messages 喂回去；顺序错了，模型看到的是乱序对话。
3. **`foreign_keys` pragma 必须开。** SQLite 历史包袱：默认 OFF。我们在 DSN 里加 `?_pragma=foreign_keys(1)`，让向不存在的 session 插 message 抛错（测试 #5 就是钉这个）。少了这一行，孤儿行会悄悄堆积。
4. **`AppendMessage` 是一个事务。** Message + 所有 Parts 一起 commit；任何一步失败整个回滚。让 `GetSession` 的不变式「读到 Message 就一定能读到它的 Parts」永远成立。

**为什么是 ~500 LOC**：因为只做四件事 —— Open / CreateSession / AppendMessage / GetSession。没有迁移框架（`CREATE TABLE IF NOT EXISTS` 够用），没有 query builder（手写 SQL），没有 connection pool tuning（`SetMaxOpenConns(1)` for `:memory:`），没有 prepared statement 缓存。每加一个机制就是 `database/sql` 的下一行而已。

## What Changed (vs. s02)

s07 是 s02 加持久化层 —— *不是* 接在 s06 后面（s06 加的是 streaming consumer，正交于持久化）。所以 diff 是相对 s02 的：

```diff
 // s02: Part 的 in-memory union，处理完即丢。
 msg := &Message{
     ID:   "msg_1",
     Role: RoleAssistant,
     Content: []Part{
         {Kind: PartText,    Text:    &TextRef{Text: "hello"}},
         {Kind: PartToolUse, ToolUse: &ToolUseRef{Name: "edit"}},
     },
 }
-// 函数返回，msg 离开作用域，没了。
-fmt.Println(msg.Content[0].Text.Text)

+// s07: 同样的 Part union，加上把它写进 SQLite 再读回来的能力。
+db, _ := Open("sessions.db")
+defer db.Close()
+_  = db.CreateSession(&Session{ID: "sess_1", Slug: "demo"})
+_  = db.AppendMessage("sess_1", msg)
+
+// 第二天、新进程、内存清零 —— Message 还能读出来。
+_, msgs, _ := db.GetSession("sess_1")
+fmt.Println(msgs[0].Parts[0].Text.Text) // "hello"
```

Part 的形状一行没改 —— 这是 s02 「Part 是 tagged union」做对了的证明。s07 加的是 *存储* 这一层，没动 *形状*。

抽象边界：s02 之前 `Message` 是 ephemeral 的；s07 加上 SQLite 之后，`Message` 变成 persisted 的 —— 但 Go 那一侧的 API 几乎不变（你还是构造 `Message{Parts: []Part{...}}`，多一行 `db.AppendMessage(sessID, m)`）。这是好的存储抽象的标志：把 *持久化* 这件事限制在显式的 CRUD 调用，不污染 in-memory 类型本身。

需要注意的小细节：s07 的 Part 比 s02 少了 `Snapshot` / `Patch` 两种 Kind —— 它们在 s10 / s14 的范围里再加。`payload` 列里的未识别 Kind 会被宽容处理（`partFromRow` 不报错，只是 variant 指针都 nil），保持 forward-compat。

## Try It

```bash
cd agents/s07-session-store

# 演示（确定性，无网络，:memory: DB）：
go run .

# 5 个测试：
go test -count=1 ./...

# vet + build + test 一把过：
go vet ./... && go build ./... && go test -count=1 ./...
```

5 个测试覆盖的场景：

1. **CreateAndGetSession** —— 9 个字段全 round-trip，时间精度到毫秒。
2. **AppendMessageWithMixedParts** —— text + tool_use + text + reasoning 四个 Part 按 *position* 顺序读出来（不是按 alphabetic 的 ID 顺序）。s06 的边界规则在持久化层重新钉一遍。
3. **AppendTwoMessagesOrdered** —— 反时序插入，按 `created_at` 顺序读出。
4. **ListSessionsNewestFirst** —— `updated_at DESC`，`limit` 生效。
5. **ForeignKeyRejectsOrphanMessage** —— 向不存在的 session 插 message 抛 FK 错；`GetSession` 不存在的 session 返 `sql.ErrNoRows`。

## Upstream Source Reading

s07 mirror 的是 opencode 的 `packages/opencode/src/session/session.ts`（schema 在隔壁 `session.sql.ts`）。整个文件 1900 行；s07 关心的是 L1-L110 —— 顶部的 imports 摆出整个 session 模块的 *依赖姿态*，然后 `fromRow` 把一个 SQLite row 翻译成 in-memory `Info` struct，是 schema → struct 的 adapter。我们用同款思路写 Go。

```ts
// upstream:packages/opencode/src/session/session.ts L1-L110

import { Slug } from "@opencode-ai/core/util/slug"
import path from "path"
import { BusEvent } from "@/bus/bus-event"
import { Bus } from "@/bus"
import { Decimal } from "decimal.js"
import { type ProviderMetadata, type LanguageModelUsage } from "ai"
import { InstallationVersion } from "@opencode-ai/core/installation/version"

import { Database } from "@/storage/db"
import { NotFoundError } from "@/storage/storage"
import { eq } from "drizzle-orm"
// ...drizzle operator imports...
import { PartTable, SessionTable } from "./session.sql"
import { ProjectTable } from "../project/project.sql"
import { Storage } from "@/storage/storage"
import * as Log from "@opencode-ai/core/util/log"
import { MessageV2 } from "./message-v2"
// ...

const log = Log.create({ service: "session" })

const parentTitlePrefix = "New session - "
const childTitlePrefix = "Child session - "

function createDefaultTitle(isChild = false) {
  return (isChild ? childTitlePrefix : parentTitlePrefix) + new Date().toISOString()
}

type SessionRow = typeof SessionTable.$inferSelect

export function fromRow(row: SessionRow): Info {
  const summary =
    row.summary_additions !== null || row.summary_deletions !== null || row.summary_files !== null
      ? {
          additions: row.summary_additions ?? 0,
          deletions: row.summary_deletions ?? 0,
          files: row.summary_files ?? 0,
          diffs: row.summary_diffs ?? undefined,
        }
      : undefined
  const share = row.share_url ? { url: row.share_url } : undefined
  const revert = row.revert ?? undefined
  return {
    id: row.id,
    slug: row.slug,
    projectID: row.project_id,
    workspaceID: row.workspace_id ?? undefined,
    directory: row.directory,
    path: row.path ?? undefined,
    parentID: row.parent_id ?? undefined,
    title: row.title,
    agent: row.agent ?? undefined,
    model: row.model
      ? {
          id: ModelID.make(row.model.id),
          providerID: ProviderID.make(row.model.providerID),
          variant: row.model.variant,
        }
      : undefined,
    version: row.version,
    summary,
    cost: row.cost,
    tokens: {
      input: row.tokens_input,
      output: row.tokens_output,
      reasoning: row.tokens_reasoning,
      cache: { read: row.tokens_cache_read, write: row.tokens_cache_write },
    },
    share, revert,
    permission: row.permission ?? undefined,
    time: {
      created: row.time_created,
      updated: row.time_updated,
      compacting: row.time_compacting ?? undefined,
      archived: row.time_archived ?? undefined,
    },
  }
}
```

逐行注释（重点行）：

- **L9-L24 imports** —— 最关键的两行：`Database from @/storage/db`（连接池）+ `PartTable, SessionTable from ./session.sql`（drizzle schema 定义）。我们 Go 那边对应 `database/sql` + 自己手写的 `CREATE TABLE` SQL。drizzle 给的是 type-safe query builder + 自动迁移；我们没有，但 schema 简单到不需要。
- **L42 logger** —— opencode 用结构化 logger（service-tagged）。我们 demo 直接 `fmt` / `log` 输出，工程代码里换成 `slog` 是显然的扩展。
- **L44-L49 default title** —— "New session - 2026-05-13T10:30:00.000Z"。我们的 Session 没 Title 字段；这个是 UI 文案，s07 没 UI 所以砍了。
- **L57 `type SessionRow = typeof SessionTable.$inferSelect`** —— drizzle 的精髓：从 schema 反推出 row 类型。Go 那边不需要这种推导，因为 schema 就是 SQL 字符串，行的字段我们手动列在 `Scan(&...)` 里。
- **L59-L110 `fromRow` adapter** —— 一个 SQLite row 翻译成 in-memory `Info` struct。三个看点：
  - **L60-L68 nullable composite field 的合并逻辑** —— `summary_additions/deletions/files` 任意一列非 null 才合成一个 `summary` 对象。Go 用 `sql.NullInt64` + 手判断，或者像我们 s07 这样直接列出 9 个字段（没有 composite，所以问题不存在）。
  - **L82-L88 JSON 反序列化** —— `row.model` 在 SQLite 是 `text({ mode: "json" })`，drizzle 自动 JSON.parse。Go 那边我们手 `json.Unmarshal`（看 `partFromRow`），效果一样。
  - **L91-L99 嵌套 tokens 字段** —— upstream 的 token 是 `{input, output, reasoning, cache: {read, write}}` 的嵌套；我们 s07 的 `Session` 把它拍平成 `InputTokens` + `OutputTokens` 两个 int（cache 列 s14 加），原因是嵌套结构 + SQLite 列名不能简单直映。
- **L112-L143 `toRow` adapter** —— 反方向，struct → row。每个 `?? null` / `?? 0` 都是 in-memory `undefined` → SQLite `NULL` / 默认值的转换。我们的 `CreateSession` 一行 SQL 直接绑参数，等价但更扁平。

permalink：

- session.ts schema + adapters（L1-L110）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L1-L110>
- session.sql.ts（drizzle table 定义）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.sql.ts#L16-L91>
- 完整 toRow（L112-L143）：<https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/session/session.ts#L112-L143>

我们留下了什么、砍了什么：

- **留下** —— 三表结构（session / message / part）、`kind` 列 + `payload` JSON 的判别 union 模式、`created_at` 排序、FK 约束、整事务的 AppendMessage、`fromRow` / `toRow` 思路（在 Go 里就是 `Scan` / `Exec` 直接绑字段）。
- **暂时砍掉** —— `workspace_id` / `directory` / `path` / `title` / `version` / `share_url` / `summary_*` / `revert` / `permission` JSON / `agent` / `model` JSON / `time_compacting` / `time_archived` 等 14 列。等到对应机制（s09 agent registry、s10 工具循环、s14 cost/recovery、未来的 share / compaction）需要时再 ALTER TABLE。
- **向前兼容** —— `Session` 结构体已经有 `Cost / InputTokens / OutputTokens` 字段，s14 加 `tokens_reasoning / tokens_cache_*` 时只是加列 + 改 SELECT/INSERT 字段列表。s10 用 `AppendMessage` 把工具循环每一轮的 Message 落盘，签名不变。s_full 的端到端把 s06 → s07 → s10 串起来时，`GetSession` 直接返还 `[]*Message`，不需要中间转换。

opencode session 层的阅读顺序：

1. `packages/opencode/src/session/session.sql.ts` L16-L91 —— drizzle schema 定义（s07 mirror 的 *表* 部分）
2. `packages/opencode/src/session/session.ts` L1-L143 —— Service init + fromRow / toRow（s07 mirror 的 *adapter* 部分，本节正文）
3. `packages/opencode/src/session/message-v2.ts` —— Part union 的完整定义（s02 已经看过基础，s07 加持久化角度复读）
4. `packages/opencode/src/session/processor.ts` L34-L150 —— 把 streaming Event reduce 进 Message 并持久化（s10 在我们这边加）
5. `packages/opencode/src/storage/db.ts` —— 连接池 / 迁移（我们 Go 那边对应 `Open` + `migrate`）
