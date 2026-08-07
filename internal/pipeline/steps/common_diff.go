package steps

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// isTestFile returns true if the file path matches common test file naming patterns.
func isTestFile(path string) bool {
	base := filepath.Base(path)
	if base == "" {
		return false
	}

	// Go: *_test.go
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	// Rust: *_test.rs
	if strings.HasSuffix(base, "_test.rs") {
		return true
	}
	// Python: test_*.py or *_test.py
	if strings.HasSuffix(base, ".py") {
		name := strings.TrimSuffix(base, ".py")
		if strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test") {
			return true
		}
	}
	// Ruby: test_*.rb
	if strings.HasSuffix(base, ".rb") && strings.HasPrefix(filepath.Base(path), "test_") {
		return true
	}
	// Java: *Test.java or *Tests.java
	if strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java") {
		return true
	}
	// JS/TS: *.test.{js,ts,jsx,tsx} or *.spec.{js,ts,jsx,tsx}
	for _, ext := range []string{".js", ".ts", ".jsx", ".tsx"} {
		if strings.HasSuffix(base, ".test"+ext) || strings.HasSuffix(base, ".spec"+ext) {
			return true
		}
	}
	return false
}

// detectNewTestFiles returns paths of new (untracked or staged-new) files that
// match common test file naming patterns. Uses git status --porcelain.
func detectNewTestFiles(ctx context.Context, dir string) []string {
	out, err := git.Run(ctx, dir, "status", "--porcelain")
	if err != nil || out == "" {
		return nil
	}
	var testFiles []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain format: XY <path> where XY is a 2-char status code + space
		status := line[:2]
		path := strings.TrimSpace(line[3:])
		// New files: untracked (??) or staged add (A ) or staged add with modifications (AM)
		if status == "??" || status[0] == 'A' {
			if isTestFile(path) {
				testFiles = append(testFiles, path)
			}
		}
	}
	return testFiles
}

// matchIgnorePattern checks if a file path matches an ignore pattern.
// Patterns follow gitignore-like semantics:
//   - No slash: match against filename only (e.g., "*.generated.go" matches "pkg/foo.generated.go")
//   - Ends with "/**": match any file under that directory (e.g., "vendor/**" matches "vendor/pkg/foo.go")
//   - Otherwise: path.Match against the full path
//
// file is always a git path, which is "/"-separated on every platform, so the
// slash-based path.Match is the matcher rather than filepath.Match: the latter
// splits on os.PathSeparator, so on Windows its "*" would run straight through
// "/" and silently widen "docs/*.md" into the whole docs subtree. The documented
// rule - "*" never crosses a "/" - is therefore the rule on every host.
func matchIgnorePattern(file, pattern string) bool {
	// "vendor/**" → matches anything under "vendor/"
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	// No slash in pattern → match against basename only
	if !strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, path.Base(file))
		return matched
	}
	// Full path match
	matched, _ := path.Match(pattern, file)
	return matched
}

// changedPathList splits a NUL-delimited `git diff --name-only -z` payload,
// preserving raw paths and git's order. The result is the complete changed set:
// callers that want the ignore-filtered subset use reviewablePaths.
func changedPathList(changedFiles string) []string {
	paths := strings.Split(changedFiles, "\x00")
	if paths[len(paths)-1] == "" {
		paths = paths[:len(paths)-1]
	}
	return paths
}

// reviewablePaths returns the changed paths that survive the repo's ignore
// patterns. ignore_patterns is a pushed-branch field, so this subset decides
// only whether a step has anything to work on; it must never decide which
// trusted configuration applies to a run.
//
// changed is the already-split complete set from changedPathList, so a step
// that needs both views splits the `git diff --name-only` payload once and
// feeds the same slice to both this and matchPathInstructions.
func reviewablePaths(changed []string, ignorePatterns []string) []string {
	var paths []string
	for _, file := range changed {
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(file, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			paths = append(paths, file)
		}
	}
	return paths
}

// filterDiff removes diff sections for files matching any of the ignore patterns.
// Input is a unified diff; output is the same diff with matching file sections removed.
// Returns the original diff unchanged if patterns is empty.
func filterDiff(diff string, patterns []string) string {
	if len(patterns) == 0 || diff == "" {
		return diff
	}

	lines := strings.Split(diff, "\n")
	var result []string
	skip := false

	for _, line := range lines {
		// Detect start of a new file section
		if strings.HasPrefix(line, "diff --git ") {
			// Extract path from "diff --git a/<path> b/<path>"
			path := extractDiffPath(line)
			skip = false
			for _, p := range patterns {
				if matchIgnorePattern(path, p) {
					skip = true
					break
				}
			}
		}
		if !skip {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// extractDiffPath extracts the file path from a "diff --git a/<path> b/<path>" header.
// For non-rename diffs both paths are identical, so we derive the path length from
// the known structure rather than splitting on " b/" which could appear in filenames.
func extractDiffPath(diffLine string) string {
	const prefix = "diff --git a/"
	rest := strings.TrimPrefix(diffLine, prefix)
	if rest == diffLine {
		return ""
	}
	// Non-rename: rest is "<path> b/<path>" where both paths are equal.
	// Total length = 2*pathLen + len(" b/") = 2*pathLen + 3.
	pathLen := (len(rest) - 3) / 2
	if pathLen > 0 && pathLen+3 <= len(rest) && rest[pathLen:pathLen+3] == " b/" {
		return rest[:pathLen]
	}
	// Fallback for renames or unexpected format: split on first " b/".
	parts := strings.SplitN(rest, " b/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}
