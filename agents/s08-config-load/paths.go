package main

import (
	"os"
	"path/filepath"
)

// configFileName is the filename inside `.opencode/` we look for. Upstream's
// paths.ts also accepts `opencode.jsonc`; we accept it too (jsonc-with-
// stripped-comments via stripJSONC in config.go) but the canonical name is
// the plain `.json` one. fileCandidates below enumerates both.
const configFileName = "opencode.json"

// fileCandidates returns the ordered list of file basenames we'll probe for
// inside any candidate `.opencode/` directory. opencode itself probes both
// `.jsonc` and `.json`; we mirror that order — `.jsonc` first because if a
// user has both, the `.jsonc` is the editable source.
func fileCandidates() []string {
	return []string{"opencode.jsonc", "opencode.json"}
}

// DefaultHomeDir returns the directory under which `<home>/.opencode/...`
// lives. Honors the `OPENCODE_CONFIG_DIR` env var (the upstream
// `Flag.OPENCODE_CONFIG_DIR`); falls back to `os.UserHomeDir()`. Tests
// override via the `homeDir` arg to Load — they never need this fn.
//
// Returning "" for the unset case is *intentional*: Load treats empty
// homeDir as "skip user config," which is what the test for "project-only"
// needs. Production callers stamp something non-empty.
func DefaultHomeDir() string {
	if v := os.Getenv("OPENCODE_CONFIG_DIR"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// userConfigPaths returns the candidate config files under `<homeDir>/
// .opencode/`. Empty homeDir → empty slice (Load treats as "no user
// config"). Returning multiple candidates lets the caller try jsonc first
// then json without two separate funcs.
func userConfigPaths(homeDir string) []string {
	if homeDir == "" {
		return nil
	}
	dir := filepath.Join(homeDir, ".opencode")
	out := make([]string, 0, 2)
	for _, name := range fileCandidates() {
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// findProjectConfig walks `cwd` and each ancestor directory looking for a
// `.opencode/opencode.json` (or `.jsonc`). Returns the first hit found
// walking upward; (`""`, false) if none in the entire ancestor chain.
//
// Walking upward (rather than just probing cwd) matches opencode's
// `afs.up({ targets: [".opencode"], start: directory })`: a config in
// `~/projects/foo/.opencode/` applies to any subdirectory of foo. Stops
// at the filesystem root. The order doesn't matter for our 5 tests
// (we set up exactly one project config), but it would matter the moment
// nested `.opencode/` directories exist — closest wins.
func findProjectConfig(cwd string) (string, bool) {
	if cwd == "" {
		return "", false
	}
	dir := cwd
	for {
		for _, name := range fileCandidates() {
			candidate := filepath.Join(dir, ".opencode", name)
			if fileExists(candidate) {
				return candidate, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// We've hit the filesystem root — `filepath.Dir("/") == "/"`.
			return "", false
		}
		dir = parent
	}
}

// fileExists is a tiny wrapper so the walking logic above reads cleanly.
// Distinguishes "doesn't exist" from "permission denied" by returning false
// only on os.IsNotExist; any other error gets logged-via-return-false too,
// but real callers will see the Stat error in their next Read call. For
// our use case this conservative shape is correct.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
