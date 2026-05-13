// upstream-reading: opencode @ 5cdbb7505efd09dfd588b732118e6f4c970c4a3d
// path: packages/opencode/src/skill/index.ts (Info schema + add() + scan() + discoverSkills(), L36-L161)
// permalink: https://github.com/sst/opencode/blob/5cdbb7505efd09dfd588b732118e6f4c970c4a3d/packages/opencode/src/skill/index.ts#L36-L161
// license: MIT — Copyright (c) 2025 opencode (LICENSE in upstream root)
//
// Why s11 cares about this file:
//   This is the *exact* file we Go-port. Three regions matter:
//
//     1. L36-L42 `Info` Schema — the runtime skill shape. We mirror the
//        load-bearing fields (name, description, location, content) and
//        add `when_to_use` as a teaching extension so CatalogString can
//        render the trigger hint as a separate "(use when: ...)" suffix.
//
//     2. L94-L131 `add()` — parse one SKILL.md, attach to state. The
//        load-bearing semantics live here:
//          - L116-L122 "duplicate skill name" → WARN and overwrite (last
//            seen wins). Our DiscoverSkills does the same — last `dirs`
//            entry wins on collision.
//          - L114 `if (!isSkillFrontmatter(md.data)) return` — frontmatter
//            without a string `name` field is silently skipped. We
//            tighten this to fail-loud (return an error) for teaching;
//            production opencode logs and continues.
//
//     3. L133-L161 `scan()` — glob a single root for `*/SKILL.md`. Our
//        Go DiscoverSkills walks each dir's direct sub-dirs (one level)
//        looking for SKILL.md. Upstream's `**` glob allows arbitrary
//        depth; in practice every real-world skill is at depth 1, so
//        for s11 we restrict to depth 1 to keep the contract obvious.
//
// What we rebuilt in Go (s11):
//   - `Info` Schema                            → `Skill` struct, 5 fields
//   - frontmatter split via ConfigMarkdown.parse → ParseSkillMD (handrolled
//                                                  `---` delimiter scan)
//   - `add(state, match, bus)` last-wins loop  → DiscoverSkills's `byName`
//                                                map
//   - `scan(state, root, pattern, opts)` glob  → os.ReadDir over each root,
//                                                check <root>/<entry>/SKILL.md
//   - `fmt(list, {verbose: false})` markdown   → CatalogString
//
// What we DID NOT rebuild yet (lives in later sessions or out of scope):
//   - external-dir discovery (.claude / .agents) and the up-walk to
//     worktree (L173-L191) — the cfg-paths discovery is enough to teach
//     the contract; up-walk is a config-paths story
//   - `cfg.skills.urls` git pull via Discovery.pull (L210-L215) — out of
//     scope; would require a git client
//   - the built-in `customize-opencode` skill (L21-L34, L250-L257) —
//     teaching focus is on disk discovery, not bundled assets
//   - Permission-filtered `available(agent)` (L277-L282) — folds into s10
//   - Effect-typed Service / Layer / Context machinery — Go uses plain
//     structs and explicit error returns
//   - Bus.publish error notifications on parse failure — we return errors
//
// The 30-60 lines below are the heart of skill discovery: the Info
// schema, the per-file add() with its dedup rule, and the per-dir
// scan() that surfaces matches. Three pieces, three roles — schema,
// per-file, per-dir.

// L36-L42 — the runtime `Info` Schema. Our Go `Skill` struct keeps
// these four (name, description, location, content) and adds a
// `WhenToUse` field for the (catalog "use when:" hint). Upstream stuffs
// the trigger hint inside `description`; s11 splits it out so
// CatalogString can render the structured one-liner.
export const Info = Schema.Struct({
  name: Schema.String,
  description: Schema.optional(Schema.String),
  location: Schema.String,                    // ★ our Path field
  content: Schema.String,                     // ★ our Body field
})
export type Info = Schema.Schema.Type<typeof Info>

// L52-L58 — the frontmatter validator. The only HARD requirement is
// `name: string`; description is optional. We mirror this in
// ParseSkillMD: missing name → error, missing description → empty string.
function isSkillFrontmatter(data: unknown): data is { name: string; description?: string } {
  return (
    isRecord(data) &&
    typeof data.name === "string" &&
    (data.description === undefined || typeof data.description === "string")
  )
}

// L94-L131 — `add()`: parse one SKILL.md and attach to the state map.
// The load-bearing semantics are:
//   1. ConfigMarkdown.parse splits frontmatter from body (L95-L98).
//      Our ParseSkillMD does the same with handrolled `---` scanning.
//   2. L114 `if (!isSkillFrontmatter(md.data)) return` — silently skip
//      malformed frontmatter. Our Go side returns an error instead, so
//      a teacher running `go test` doesn't have to wonder why a file
//      was silently ignored.
//   3. L116-L122 "duplicate skill name" — log a WARN, then OVERWRITE.
//      Last-write-wins. Our DiscoverSkills replicates this: later
//      entries in `dirs` overwrite earlier same-named ones.
//   4. L125-L130 — store name → Info, where Info has (name, description,
//      location, content). Same shape as our Skill struct.
const add = Effect.fnUntraced(function* (state: State, match: string, bus: Bus.Interface) {
  const md = yield* Effect.tryPromise({
    try: () => ConfigMarkdown.parse(match),    // ★ frontmatter + body split
    catch: (err) => err,
  }).pipe(
    Effect.catch(
      Effect.fnUntraced(function* (err) {
        const message = ConfigMarkdown.FrontmatterError.isInstance(err)
          ? err.data.message
          : `Failed to parse skill ${match}`
        // upstream logs + publishes; we'd return error in Go
        return undefined
      }),
    ),
  )
  if (!md) return
  if (!isSkillFrontmatter(md.data)) return                         // ← silently skipped upstream

  if (state.skills[md.data.name]) {
    log.warn("duplicate skill name", {                              // ★ last-wins:
      name: md.data.name,                                           //   warn then overwrite
      existing: state.skills[md.data.name].location,
      duplicate: match,
    })
  }

  state.dirs.add(path.dirname(match))
  state.skills[md.data.name] = {                                    // ← store the Info
    name: md.data.name,
    description: md.data.description,
    location: match,
    content: md.content,
  }
})

// L133-L161 — `scan()`: glob a single root for SKILL.md files at any
// depth, accumulate matches and parent dirs into the ScanState. Upstream
// uses `Glob.scan` with patterns like `"skills/**/SKILL.md"` (depth 2+)
// or `"**/SKILL.md"` (any depth). Our Go DiscoverSkills restricts to
// depth-1 (`<root>/<sub>/SKILL.md`) for teaching simplicity — every
// real-world skill lives at depth 1, and the test surface stays small.
const scan = Effect.fnUntraced(function* (
  state: ScanState,
  root: string,
  pattern: string,                              // upstream: "skills/**/SKILL.md" | "**/SKILL.md"
  opts?: { dot?: boolean; scope?: string },
) {
  const matches = yield* Effect.tryPromise({
    try: () =>
      Glob.scan(pattern, {                      // ★ the actual glob walk
        cwd: root,
        absolute: true,
        include: "file",
        symlink: true,
        dot: opts?.dot,
      }),
    catch: (error) => error,
  })

  for (const match of matches) {                // ★ accumulate per-file path
    state.matches.add(match)
    state.dirs.add(path.dirname(match))
  }
})

// END EXCERPT — the file continues with discoverSkills (L163-L221, the
// outer orchestrator that scans external dirs + config dirs + URL-pulled
// dirs), loadSkills (L223-L230, the per-match parse fan-out), the
// Effect-typed Service/Layer wiring (L232-L294), and the fmt() catalog
// renderer (L296-L321). Our Go side ports the schema + last-wins
// semantics + glob + catalog format; everything else is environment
// (Effect runtime, bus events, config plumbing) that doesn't translate.
