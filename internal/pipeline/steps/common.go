package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Finding represents a single code review or lint finding.
type Finding = types.Finding

// Findings is the structured output from a review or lint agent call.
type Findings = types.Findings

// resolveBaseSHA returns a usable base SHA for diff/log operations.
// When baseSHA is the zero ref (new branch push), it tries git merge-base
// against the default branch, falling back to the empty tree SHA.
func resolveBaseSHA(ctx context.Context, workDir, baseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(baseSHA) {
		return baseSHA
	}
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return git.EmptyTreeSHA
}

// resolveBranchBaseSHA returns the branch base commit relative to the default
// branch when possible. This keeps pipeline steps scoped to the full branch,
// not just the last pushed delta. If merge-base cannot be determined, it falls
// back to resolveBaseSHA.
func resolveBranchBaseSHA(ctx context.Context, workDir, fallbackBaseSHA, defaultBranch string) string {
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, defaultBranch)
}

func mergeBaseWithDefaultBranch(ctx context.Context, workDir, defaultBranch string) string {
	if strings.TrimSpace(defaultBranch) == "" {
		return ""
	}
	for _, ref := range []string{"origin/" + defaultBranch, defaultBranch} {
		mb, err := git.Run(ctx, workDir, "merge-base", "HEAD", ref)
		if err == nil && strings.TrimSpace(mb) != "" {
			return strings.TrimSpace(mb)
		}
	}
	return ""
}

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// runShellCommand executes a shell command and returns stdout+stderr, exit code, and error.
// A non-zero exit code is not treated as an error — only exec failures return error.
func runShellCommand(ctx context.Context, dir, cmdStr string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode(), nil
		}
		return "", -1, fmt.Errorf("run command %q: %w", cmdStr, err)
	}
	return string(out), 0, nil
}

// findingsSchema is the JSON schema for structured findings output.
var findingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"}
				},
				"required": ["severity", "description"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

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
//   - Otherwise: filepath.Match against the full path
func matchIgnorePattern(path, pattern string) bool {
	// "vendor/**" → matches anything under "vendor/"
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	// No slash in pattern → match against basename only
	if !strings.Contains(pattern, "/") {
		base := filepath.Base(path)
		matched, _ := filepath.Match(pattern, base)
		return matched
	}
	// Full path match
	matched, _ := filepath.Match(pattern, path)
	return matched
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

// AllSteps returns the fixed pipeline step sequence.
func AllSteps() []pipeline.Step {
	return []pipeline.Step{
		&RebaseStep{},
		&ReviewStep{},
		&TestStep{},
		&LintStep{},
		&PushStep{},
		&PRStep{},
		&BabysitStep{},
	}
}
