package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is the runtime shape of one SKILL.md file on disk. Mirrors
// upstream's `Skill.Info` Schema in
// `packages/opencode/src/skill/index.ts` L36-L42 — same load-bearing
// fields (name, description, body) plus the file `Path` so the demo
// (and a future tool that surfaces "where did this skill come from")
// can render the source location.
//
// WhenToUse is a teaching-extension on the upstream shape: upstream
// stuffs the "use this skill when…" hint inside `description` itself.
// We split it out so CatalogString can render a structured one-liner
// — easier to grep, easier to assert in tests.
//
// Body is the markdown content AFTER the closing `---` of the
// frontmatter. The system-prompt builder in s10's loop only needs the
// (name, description, when_to_use) triple to advertise the skill; Body
// is loaded lazily by the model via a (future) `read_skill` tool. We
// keep it in the struct so the demo can show the full file content.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when_to_use"`
	Body        string `yaml:"-"`
	Path        string `yaml:"-"`
}

// frontmatterDelim is the exact 3-byte sequence that opens/closes a YAML
// frontmatter block in markdown. Kept as a named constant because the
// parser checks it three times (open, close, "anything past close = body")
// and a naked "---" string-literal in three places invites typos.
const frontmatterDelim = "---"

// ParseSkillMD splits a SKILL.md file into (frontmatter, body) and
// yaml-unmarshals the frontmatter into a Skill.
//
// Contract:
//   - File MUST start with a `---` line. Anything else returns an error
//     (we don't try to be clever about leading whitespace or BOMs —
//     upstream's `ConfigMarkdown.parse` is equally strict).
//   - Closing `---` line is mandatory. Missing close → error.
//   - Frontmatter content is YAML; missing `name` is treated as a parse
//     error so callers see "this isn't a valid skill" instead of a
//     silent skip.
//   - Body is everything after the closing `---` (newline-trimmed at the
//     leading edge so the body doesn't start with a stray blank line).
//
// The `path` argument is informational — it goes into Skill.Path and
// surfaces in error messages so a misformatted file's location is
// always clear.
func ParseSkillMD(path string, content []byte) (*Skill, error) {
	// Normalize line endings: a Windows-edited skill file with \r\n
	// would otherwise break the literal "---\n" split. Cheap to do
	// once, immune to platform drift.
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))

	lines := bytes.SplitN(content, []byte("\n"), 2)
	if len(lines) < 2 || strings.TrimSpace(string(lines[0])) != frontmatterDelim {
		return nil, fmt.Errorf("skill: %s: missing leading %q frontmatter delimiter", path, frontmatterDelim)
	}

	// Find the closing "---" line. We scan line-by-line (rather than
	// byte-search for "\n---\n") so a `---` that appears at the very
	// end of the file with no trailing newline still terminates the
	// block.
	rest := lines[1]
	closeIdx := findClosingDelim(rest)
	if closeIdx < 0 {
		return nil, fmt.Errorf("skill: %s: missing closing %q frontmatter delimiter", path, frontmatterDelim)
	}

	frontmatter := rest[:closeIdx]
	body := ""
	if closeIdx < len(rest) {
		// Skip the closing delimiter line itself (and its trailing \n
		// if present) so Body starts at the first byte of real content.
		afterClose := rest[closeIdx:]
		// afterClose begins with "---"; advance past the whole line.
		nl := bytes.IndexByte(afterClose, '\n')
		if nl >= 0 {
			body = strings.TrimLeft(string(afterClose[nl+1:]), "\n")
		}
	}

	s := &Skill{Path: path, Body: body}
	if err := yaml.Unmarshal(frontmatter, s); err != nil {
		return nil, fmt.Errorf("skill: %s: yaml unmarshal: %w", path, err)
	}
	if strings.TrimSpace(s.Name) == "" {
		return nil, fmt.Errorf("skill: %s: frontmatter missing required %q field", path, "name")
	}
	return s, nil
}

// findClosingDelim returns the byte offset of the line that contains
// just `---` (after whitespace trim) within `data`, or -1 if none.
// We return the offset of the start-of-line for that delimiter so the
// caller can slice [0:offset] to get the frontmatter content.
func findClosingDelim(data []byte) int {
	offset := 0
	for offset < len(data) {
		nl := bytes.IndexByte(data[offset:], '\n')
		var line []byte
		var lineEnd int
		if nl < 0 {
			line = data[offset:]
			lineEnd = len(data)
		} else {
			line = data[offset : offset+nl]
			lineEnd = offset + nl
		}
		if strings.TrimSpace(string(line)) == frontmatterDelim {
			return offset
		}
		if nl < 0 {
			return -1
		}
		offset = lineEnd + 1
	}
	return -1
}

// DiscoverSkills walks each directory in `dirs` looking for one-level-
// deep `SKILL.md` files — i.e. `<dir>/<skill-name>/SKILL.md`. Returns
// every successfully-parsed Skill in the order
// (dirs[0]'s skills, sorted by dir entry name) ++
// (dirs[1]'s skills, sorted) ++ …  with **last-wins on duplicate name**:
// if two dirs both ship a skill called "git-helper", the one from the
// LATER dir in `dirs` wins. The earlier one is silently dropped.
//
// Why one level deep (not arbitrary nesting): mirrors upstream's
// `EXTERNAL_SKILL_PATTERN = "skills/**/SKILL.md"` — the `**` glob
// allows arbitrary depth, but every real-world skill lives at exactly
// one level (skills/<name>/SKILL.md). Restricting to one level keeps
// the test surface small and the discovery contract obvious.
//
// Why last-wins: matches upstream's L116-L122 — when `state.skills`
// already has an entry under that name, upstream WARNS but overwrites.
// Order in upstream is "global dir" → "project dir" → "user-config
// dirs" → "url-pulled dirs"; later sources override earlier ones.
// We make the same call: caller passes dirs in priority order, lowest
// first, highest last; the last one wins.
//
// Errors: a missing dir is NOT an error (mirroring upstream's
// `if (!isDir(root)) continue`). A SKILL.md that fails to parse
// returns the error wrapped with file path; callers can choose to
// log-and-continue (production) or fail-loud (tests). We choose
// fail-loud here for a teaching contract — caller doesn't have to
// guess whether a malformed file was silently skipped.
func DiscoverSkills(dirs []string) ([]*Skill, error) {
	// byName tracks the latest registration; later dirs overwrite.
	// We also keep `order` to preserve the relative order of skills
	// from the same dir (sorted by sub-dir name) and across dirs
	// (later dir's entries replace earlier ones in-place — but we
	// rebuild `order` from `byName` at the end so duplicates don't
	// linger).
	byName := make(map[string]*Skill)
	var firstSeenOrder []string

	for _, root := range dirs {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("skill: read dir %q: %w", root, err)
		}
		// Sort sub-dir entries by name for deterministic discovery
		// order within a single root. (os.ReadDir sorts lexically as
		// of Go 1.16, but we sort explicitly so the contract isn't
		// implicit in the stdlib behavior.)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillFile := filepath.Join(root, entry.Name(), "SKILL.md")
			info, err := os.Stat(skillFile)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("skill: stat %q: %w", skillFile, err)
			}
			if info.IsDir() {
				continue
			}
			content, err := os.ReadFile(skillFile)
			if err != nil {
				return nil, fmt.Errorf("skill: read %q: %w", skillFile, err)
			}
			s, err := ParseSkillMD(skillFile, content)
			if err != nil {
				return nil, err
			}
			if _, seen := byName[s.Name]; !seen {
				firstSeenOrder = append(firstSeenOrder, s.Name)
			}
			byName[s.Name] = s
		}
	}

	out := make([]*Skill, 0, len(byName))
	for _, name := range firstSeenOrder {
		out = append(out, byName[name])
	}
	return out, nil
}

// CatalogString renders a system-prompt-ready listing of the skills,
// one per line. Format:
//
//	- <name>: <description> (use when: <when_to_use>)
//
// Mirrors upstream's `fmt(...)` non-verbose branch at L315-L320 — same
// "- **name**: description" idea, extended to also surface when_to_use
// because s11's Skill struct splits the trigger hint out as its own
// field. If a skill has no Description, the field renders empty (we
// don't second-guess the author's choice). If WhenToUse is empty, the
// "(use when: …)" suffix is omitted entirely so the catalog stays
// readable for skills that don't bother declaring a trigger.
//
// Returns the empty string for an empty input slice. (System-prompt
// templates can do `if cat := CatalogString(s); cat != "" { ... }` to
// gate the whole "Available Skills:" section.)
//
// Order: skills are listed in the slice's order (DiscoverSkills's
// stable order: per-dir sorted by sub-dir name, dirs in argument order
// with last-wins on duplicates). We do NOT re-sort — DiscoverSkills's
// contract is the load-bearing one; if a caller wants alphabetical,
// they can sort the slice themselves before calling CatalogString.
func CatalogString(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	for i, s := range skills {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Description)
		if strings.TrimSpace(s.WhenToUse) != "" {
			b.WriteString(" (use when: ")
			b.WriteString(s.WhenToUse)
			b.WriteString(")")
		}
	}
	return b.String()
}

// ErrNoSkillsDir is returned by no callers in s11 — kept here so a
// future caller that wants to distinguish "you didn't pass any dirs"
// from "you passed dirs and none had skills" has a sentinel to compare
// against. (s11 itself returns an empty slice in both cases, by design.)
var ErrNoSkillsDir = errors.New("skill: no skill directories provided")
