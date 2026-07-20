package steps

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// coverageProvider is the per-language extension point for the coverage-aware
// test step. One implementation per language, each in its own file
// (coverage_go.go, coverage_js.go, coverage_swift.go), self-registering in
// init() via registerCoverageProvider. The dispatcher (runCoverageCheck in
// coverage.go) loops over registered providers and feeds each into the shared,
// language-neutral uncoveredChangedLineFindings core, so a new language is an
// isolated file that touches no shared code.
type coverageProvider interface {
	// Name is the language id used in finding IDs and logs ("go", "js", "swift").
	Name() string

	// Active reports whether this language applies to the worktree — typically a
	// manifest/gate file check (go.mod | package.json | Package.swift |
	// *.xcodeproj). The dispatcher runs a provider only when Active==true, so
	// language detection lives in the provider, not in a central if/else tree.
	Active(workDir string, env []string) bool

	// CoverableChangedFiles filters the repo-relative changed-file list (the keys
	// `git diff --name-only` emits) to this language's accountable source files:
	// language extension, excludes test files, applies ignore patterns, drops
	// deletions. Each provider owns its test-file rule (Go: *_test.go; JS:
	// *.test.*/*.spec.*/__tests__; Swift: *Tests/*).
	CoverableChangedFiles(changed []string, workDir string, ignorePatterns []string) []string

	// RunCoverage collects whole-worktree coverage in the language's NATIVE raw
	// format and returns it as a string (coverprofile text, coverage-final.json,
	// or xccov/llvm-cov JSON). testedCmd is the human-readable command surfaced
	// in the "tested" log. Errors degrade to a logged no-op — never block the
	// pipeline (the dispatcher logs and continues).
	RunCoverage(sctx *pipeline.StepContext) (raw string, testedCmd string, err error)

	// ParseBlocks parses the native raw output into per-file coverBlocks keyed by
	// REPO-RELATIVE POSIX path — the same key space `git diff` emits. The shared
	// core then intersects these with the added-line ranges. This is where path
	// normalization happens (strip module/abs-workDir/build-relative prefixes).
	// Use toRepoRelPOSIX so the path-key invariant is encoded once.
	ParseBlocks(raw string, workDir string) map[string][]coverBlock
}

// coverageProviders is the registry of all coverage providers, populated by
// each provider's init() via registerCoverageProvider. The dispatcher loops
// over this slice; a new language is added by dropping in a file that calls
// registerCoverageProvider in init() and touches nothing else.
var coverageProviders []coverageProvider

// registerCoverageProvider adds a provider to the registry. Called from each
// provider's init(); init() ordering is guaranteed single-threaded by the Go
// runtime, so concurrent registration is not a concern.
func registerCoverageProvider(p coverageProvider) {
	coverageProviders = append(coverageProviders, p)
}

// namespaceFindings rewrites each finding ID from
// `uncovered-changed-lines:<path>` to `uncovered-changed-lines:<lang>:<path>`,
// so multi-language repos keep distinct, filterable finding identities. The
// bare prefix is preserved, so downstream TUI/filter matching that keys off
// `uncovered-changed-lines:` keeps working — <lang> is inserted after it.
func namespaceFindings(lang string, fs []Finding) []Finding {
	for i := range fs {
		fs[i].ID = strings.Replace(fs[i].ID,
			uncoveredChangedLinesIDPrefix,
			uncoveredChangedLinesIDPrefix+lang+":", 1)
	}
	return fs
}

// toRepoRelPOSIX normalizes a native path (absolute or relative) to the
// repo-relative POSIX key space that `git diff --name-only` emits. This is the
// #1 correctness invariant for every provider's ParseBlocks: the coverBlock map
// keys must be byte-identical to git's repo-relative POSIX paths, because both
// the coverable-changed-file list and the added-line-range map are keyed that
// way. Encoded once here so every provider reuses it.
//
// It strips an optional workDir prefix (handling both the literal and the
// symlink-resolved spelling, e.g. macOS /var vs /private/var), Clean()s the
// result, and converts the OS path separator to "/". A path that equals workDir
// becomes "."; a path not under workDir is returned cleaned and POSIX-ized
// (still absolute) so the caller can detect and drop it.
func toRepoRelPOSIX(absPath, workDir string) string {
	for _, p := range pathSpellings(absPath) {
		for _, root := range pathSpellings(workDir) {
			if p == root {
				return "."
			}
			if strings.HasPrefix(p, root+string(filepath.Separator)) {
				return filepath.ToSlash(filepath.Clean(strings.TrimPrefix(p, root+string(filepath.Separator))))
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(absPath))
}

// pathSpellings returns the cleaned path plus its symlink-resolved form when
// they differ and resolution succeeds. Used by toRepoRelPOSIX so a path matches
// whether it is spelled via the symlink (e.g. macOS /var) or its target
// (/private/var), regardless of which side (coverage tool vs workDir) uses
// which spelling.
func pathSpellings(p string) []string {
	clean := filepath.Clean(p)
	out := []string{clean}
	if p == "" {
		return out
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		resolved = filepath.Clean(resolved)
		if resolved != clean {
			out = append(out, resolved)
		}
	}
	return out
}
