package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
		if runtime.GOOS == "windows" && len(entry) > len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			return entry[len(prefix):], true
		}
	}
	return "", false
}

func executableCandidates(name string, env []string) []string {
	candidates := []string{name}
	if runtime.GOOS != "windows" || filepath.Ext(name) != "" {
		return candidates
	}
	pathExt := ".COM;.EXE;.BAT;.CMD"
	if customPathExt, ok := envValue(env, "PATHEXT"); ok && strings.TrimSpace(customPathExt) != "" {
		pathExt = customPathExt
	}
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		candidates = append(candidates, name+ext)
	}
	return candidates
}

func findInCustomPath(env []string, name string) string {
	customPath, ok := envValue(env, "PATH")
	if !ok {
		return ""
	}
	for _, dir := range filepath.SplitList(customPath) {
		for _, candidateName := range executableCandidates(name, env) {
			candidate := filepath.Join(dir, candidateName)
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				return candidate
			}
		}
	}
	return ""
}

func copyDirContents(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())
		if err := copyPath(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(srcPath, dstPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		return os.Symlink(target, dstPath)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dstPath, info.Mode().Perm()); err != nil {
			return err
		}
		return copyDirContents(srcPath, dstPath)
	}
	return copyFile(srcPath, dstPath, info.Mode().Perm())
}

func copyFile(srcPath, dstPath string, perm os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return nil
}

// stepCmd creates an exec.Cmd that inherits the StepContext's extra Env, if any.
// When sctx.Env overrides PATH, the binary is resolved from the overridden PATH
// so that tests can inject fake binaries without modifying the process environment.
func stepCmd(sctx *pipeline.StepContext, name string, args ...string) *exec.Cmd {
	resolved := name
	if len(sctx.Env) > 0 && !strings.Contains(name, string(filepath.Separator)) {
		if candidate := findInCustomPath(sctx.Env, name); candidate != "" {
			resolved = candidate
		}
	}
	cmd := exec.CommandContext(sctx.Ctx, resolved, args...)
	cmd.Dir = sctx.WorkDir
	if len(sctx.Env) > 0 {
		cmd.Env = append(os.Environ(), sctx.Env...)
	}
	return cmd
}

// stepGitRun runs a git command using the StepContext's environment.
// It is like git.Run but respects sctx.Env so that tests can inject a fake git binary.
func stepGitRun(sctx *pipeline.StepContext, args ...string) (string, error) {
	cmd := stepCmd(sctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// stepCLIAvailable checks whether the provider CLI binary is available,
// respecting any custom PATH in sctx.Env.
func stepCLIAvailable(sctx *pipeline.StepContext, provider scm.Provider) bool {
	name := provider.CLIName()
	if name == "" {
		return false
	}
	if len(sctx.Env) == 0 {
		return scm.CLIAvailable(provider)
	}
	if candidate := findInCustomPath(sctx.Env, name); candidate != "" {
		return true
	}
	_, ok := envValue(sctx.Env, "PATH")
	if ok {
		return false
	}
	return scm.CLIAvailable(provider)
}

// stepAuthConfigured checks whether the provider CLI is authenticated,
// using sctx.Env to resolve the binary and pass environment variables.
func stepAuthConfigured(sctx *pipeline.StepContext, provider scm.Provider) bool {
	args := provider.AuthCheckCommand()
	if len(args) == 0 {
		return false
	}
	cmd := stepCmd(sctx, args[0], args[1:]...)
	return cmd.Run() == nil
}

// runShellCommand executes a shell command and returns stdout+stderr, exit code, and error.
// A non-zero exit code is not treated as an error — only exec failures return error.
func runShellCommand(ctx context.Context, dir, cmdStr string) (string, int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
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
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

var commitSummarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string"}
	},
	"required": ["summary"]
}`)

type commitSummary struct {
	Summary string `json:"summary"`
}

func normalizedBranchRef(ref string) string {
	if !strings.HasPrefix(ref, "refs/") {
		return "refs/heads/" + ref
	}
	return ref
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	ctx := sctx.Ctx
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	if summary == "" {
		summary = fallbackSummary
	}
	commitMessage := deterministicFixCommitMessage(stepName, summary)
	if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", commitMessage); err != nil {
		return fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	return nil
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func deterministicFixCommitMessage(stepName types.StepName, summary string) string {
	if summary == "" {
		summary = "apply fixes"
	}
	return fmt.Sprintf("no-mistakes(%s): %s", stepName, summary)
}

// reviewFindingsSchema is the JSON schema for structured review output with risk assessment.
// Field order matters for chain-of-thought: findings first, then risk level, then rationale.
var reviewFindingsSchema = json.RawMessage(`{
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
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"risk_level": {"type": "string", "enum": ["low", "medium", "high"]},
		"risk_rationale": {"type": "string"}
	},
	"required": ["findings", "risk_level", "risk_rationale"]
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
		&DocumentStep{},
		&LintStep{},
		&PushStep{},
		&PRStep{},
		&CIStep{},
	}
}
