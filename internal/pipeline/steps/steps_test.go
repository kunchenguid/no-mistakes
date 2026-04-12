package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestMain handles fake CLI dispatch when the test binary is invoked as gh/glab.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_CLI_MODE"); mode != "" {
		handleFakeCLI(mode)
		return
	}
	os.Exit(m.Run())
}

func handleFakeCLI(mode string) {
	args := os.Args[1:]
	logFile := os.Getenv("FAKE_CLI_LOG")

	if logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
	}

	switch mode {
	case "gh":
		fakeGHHandler(args)
	case "glab":
		fakeGlabHandler(args)
	case "babysit-gh":
		fakeBabysitGHHandler(args)
	case "babysit-gh-nochecks":
		fakeBabysitGHNoChecksHandler(args)
	default:
		os.Exit(1)
	}
}

func fakeGHHandler(args []string) {
	prURL := os.Getenv("FAKE_CLI_PR_URL")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
		if prURL != "" {
			fmt.Println(prURL)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "edit" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
		fmt.Println("https://github.com/test/repo/pull/99")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeGlabHandler(args []string) {
	mrViewJSON := os.Getenv("FAKE_CLI_MR_VIEW_JSON")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "view" {
		if mrViewJSON != "" {
			fmt.Println(mrViewJSON)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "update" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "create" {
		fmt.Println("https://gitlab.com/test/repo/-/merge_requests/99")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeBabysitGHHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	commentsJSON := os.Getenv("FAKE_CLI_COMMENTS")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println(state)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json comments") {
		fmt.Println(commentsJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr comment") {
		os.Exit(0)
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeBabysitGHNoChecksHandler(args []string) {
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Fprintln(os.Stderr, "no checks reported on the 'feature/e2e' branch")
		os.Exit(1)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println("OPEN")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json comments") {
		fmt.Println("[]")
		os.Exit(0)
	}
	os.Exit(1)
}

// --- mock agent ---

type mockAgent struct {
	name  string
	runFn func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	calls []agent.RunOpts
}

func (m *mockAgent) Name() string { return m.name }
func (m *mockAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	m.calls = append(m.calls, opts)
	if m.runFn != nil {
		return m.runFn(ctx, opts)
	}
	return &agent.Result{}, nil
}
func (m *mockAgent) Close() error { return nil }

// --- git repo helpers ---

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "status", "--porcelain")
}

func lastCommitMessage(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "log", "-1", "--pretty=%s")
}

// setupGitRepo creates a git repo with a base commit on main and a head commit on feature.
// Returns (repoDir, baseSHA, headSHA).
func setupGitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()

	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")

	// Base commit
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base content"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Feature branch with changes
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	return dir, baseSHA, headSHA
}

// newTestContext creates a StepContext for testing with optional config overrides.
func newTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return &pipeline.StepContext{
		Ctx:     context.Background(),
		Run:     &db.Run{ID: "run-1", RepoID: "repo-1", Branch: "refs/heads/feature", HeadSHA: headSHA, BaseSHA: baseSHA},
		Repo:    &db.Repo{ID: "repo-1", WorkingPath: workDir, UpstreamURL: "https://github.com/test/repo", DefaultBranch: "main"},
		WorkDir: workDir,
		Agent:   ag,
		Config:  &config.Config{Agent: types.AgentClaude, Commands: cmds},
		DB:      database,
		Log:     func(s string) {},
	}
}

// --- common tests ---

func TestIsZeroSHA(t *testing.T) {
	tests := []struct {
		sha  string
		want bool
	}{
		{"0000000000000000000000000000000000000000", true},
		{"abc123", false},
		{"", false},
		{"00000", false},
	}
	for _, tt := range tests {
		if got := git.IsZeroSHA(tt.sha); got != tt.want {
			t.Errorf("IsZeroSHA(%q) = %v, want %v", tt.sha, got, tt.want)
		}
	}
}

func TestResolveBaseSHA_NonZero(t *testing.T) {
	dir := t.TempDir()
	got := resolveBaseSHA(context.Background(), dir, "abc123", "main")
	if got != "abc123" {
		t.Errorf("resolveBaseSHA non-zero = %q, want abc123", got)
	}
}

func TestResolveBaseSHA_ZeroWithMergeBase(t *testing.T) {
	// Create a repo with main branch and feature branch diverging
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	mainSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("feat"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature commit")

	zeroSHA := "0000000000000000000000000000000000000000"
	got := resolveBaseSHA(context.Background(), dir, zeroSHA, "main")
	if got != mainSHA {
		t.Errorf("resolveBaseSHA zero with merge-base = %q, want %q", got, mainSHA)
	}
}

func TestResolveBaseSHA_ZeroNoDefaultBranch(t *testing.T) {
	// Repo with no "main" branch — should fall back to empty tree SHA
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("data"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")

	zeroSHA := "0000000000000000000000000000000000000000"
	got := resolveBaseSHA(context.Background(), dir, zeroSHA, "main")
	if got != git.EmptyTreeSHA {
		t.Errorf("resolveBaseSHA zero no default = %q, want %q", got, git.EmptyTreeSHA)
	}
}

func TestHasBlockingFindings(t *testing.T) {
	tests := []struct {
		name     string
		items    []Finding
		expected bool
	}{
		{"empty", nil, false},
		{"info only", []Finding{{Severity: "info", Description: "note"}}, false},
		{"warning", []Finding{{Severity: "warning", Description: "warn"}}, true},
		{"error", []Finding{{Severity: "error", Description: "err"}}, true},
		{"mixed", []Finding{{Severity: "info"}, {Severity: "error"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasBlockingFindings(tt.items); got != tt.expected {
				t.Errorf("hasBlockingFindings() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRunShellCommand(t *testing.T) {
	dir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		out, code, err := runShellCommand(context.Background(), dir, "echo hello")
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(out, "hello") {
			t.Errorf("output = %q, want to contain 'hello'", out)
		}
	})

	t.Run("nonzero exit", func(t *testing.T) {
		_, code, err := runShellCommand(context.Background(), dir, "exit 42")
		if err != nil {
			t.Fatal(err)
		}
		if code != 42 {
			t.Errorf("exit code = %d, want 42", code)
		}
	})
}

func TestAllSteps(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 7 {
		t.Fatalf("AllSteps() returned %d steps, want 7", len(steps))
	}
	expected := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepLint, types.StepPush, types.StepPR, types.StepBabysit}
	for i, s := range steps {
		if s.Name() != expected[i] {
			t.Errorf("step %d name = %s, want %s", i, s.Name(), expected[i])
		}
	}
}

func TestRebaseStep_Name(t *testing.T) {
	s := &RebaseStep{}
	if s.Name() != types.StepRebase {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepRebase)
	}
}

func TestRebaseStep_RebasesOntoDefaultBranch(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature commit")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("base\nmain\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main update")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval for clean rebase")
	}
	if sctx.Run.HeadSHA == originalHeadSHA {
		t.Fatal("expected head SHA to change after rebase")
	}
	mergeBase := gitCmd(t, dir, "merge-base", "HEAD", "origin/main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	if mergeBase != originMain {
		t.Fatalf("merge-base = %s, want origin/main %s", mergeBase, originMain)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.HeadSHA != sctx.Run.HeadSHA {
		t.Fatalf("stored head SHA = %v, want %s", stored, sctx.Run.HeadSHA)
	}
}

// --- Review step tests ---

func TestReviewStep_Name(t *testing.T) {
	s := &ReviewStep{}
	if s.Name() != types.StepReview {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepReview)
	}
}

func TestReviewStep_EmptyDiff(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	sha := gitCmd(t, dir, "rev-parse", "HEAD")

	// Same base and head — empty diff
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, sha, sha, config.Commands{})

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for empty diff")
	}
	if len(ag.calls) != 0 {
		t.Error("expected no agent calls for empty diff")
	}
}

func TestReviewStep_WithWarnings(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{
		Items:   []Finding{{Severity: "warning", Description: "potential null pointer"}},
		Summary: "found 1 issue",
	}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed for warning findings")
	}
	if outcome.Findings == "" {
		t.Error("expected findings to be set")
	}
	if len(ag.calls) != 1 {
		t.Errorf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, baseSHA) {
		t.Error("expected prompt to contain base SHA")
	}
	if !strings.Contains(ag.calls[0].Prompt, headSHA) {
		t.Error("expected prompt to contain head SHA")
	}
	if !strings.Contains(ag.calls[0].Prompt, "refs/heads/feature") {
		t.Error("expected prompt to contain branch name")
	}
	if strings.Contains(ag.calls[0].Prompt, "Diff:\n") {
		t.Error("expected prompt to avoid embedding the diff")
	}
	if strings.Contains(ag.calls[0].Prompt, "feature code") {
		t.Error("expected prompt to avoid embedding diff contents")
	}
	if !strings.Contains(ag.calls[0].Prompt, "focus only on changed code") {
		t.Error("expected prompt to constrain review scope to changed code")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Only comment on things that genuinely matter") {
		t.Error("expected prompt to discourage low-signal findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, "No generic advice like \"add more tests\" or \"improve docs\"") {
		t.Error("expected prompt to ban generic review advice")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do NOT report styling, formatting, linting, compilation, or type-checking issues") {
		t.Error("expected prompt to exclude lint/style/type findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Anchor every finding to a specific file and one-indexed line number in the changed code") {
		t.Error("expected prompt to require anchored findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, "severity \"error\" for problems that should absolutely not get merged") {
		t.Error("expected prompt to map severities to Airlock-style critique categories")
	}
	if !strings.Contains(ag.calls[0].Prompt, "return an empty findings array") {
		t.Error("expected prompt to allow empty findings when clean")
	}
}

func TestReviewStep_Clean(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{
		Items:   []Finding{{Severity: "info", Description: "looks good"}},
		Summary: "no issues found",
	}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for info-only findings")
	}
}

func TestReviewStep_AgentError(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("agent crashed")
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "agent review") {
		t.Errorf("error = %v, want to contain 'agent review'", err)
	}
}

func TestReviewStep_ZeroBaseSHA(t *testing.T) {
	// New branch scenario: baseSHA is all-zeros
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findings := Findings{Items: nil, Summary: "clean"}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}

	zeroSHA := "0000000000000000000000000000000000000000"
	sctx := newTestContext(t, ag, dir, zeroSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval for clean review")
	}
	// Verify agent was called with commit metadata instead of an inline diff.
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, headSHA) {
		t.Error("expected prompt to contain head SHA")
	}
	if !strings.Contains(ag.calls[0].Prompt, "refs/heads/feature") {
		t.Error("expected prompt to contain branch name")
	}
	if strings.Contains(ag.calls[0].Prompt, "feature code") {
		t.Error("expected prompt to avoid embedding diff contents")
	}
}

func TestReviewStep_ExistingBranchUsesMergeBaseScope(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	mergeBaseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "first feature commit")
	oldRemoteSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "second feature commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findingsJSON, _ := json.Marshal(Findings{Summary: "clean"})
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, oldRemoteSHA, headSHA, config.Commands{})

	step := &ReviewStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, mergeBaseSHA) {
		t.Errorf("expected prompt to contain merge-base SHA %s", mergeBaseSHA)
	}
	if strings.Contains(ag.calls[0].Prompt, oldRemoteSHA) {
		t.Errorf("expected prompt to avoid push old SHA %s", oldRemoteSHA)
	}
}

func TestReviewStep_FixMode(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"summary":"address review findings"}`)}, nil
			}
			// Review call — return clean findings
			findings := Findings{Items: nil, Summary: "all clear"}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"possible nil dereference"}],"summary":"1 issue"}`

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed after fix")
	}
	if callCount != 2 {
		t.Errorf("expected 2 agent calls (fix + review), got %d", callCount)
	}
	if !strings.Contains(ag.calls[0].Prompt, baseSHA) {
		t.Error("expected fix prompt to contain base SHA")
	}
	if !strings.Contains(ag.calls[0].Prompt, headSHA) {
		t.Error("expected fix prompt to contain head SHA")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Investigate previous review findings and address legitimate ones") {
		t.Error("expected review fix prompt to frame the task around previous findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Always start with double checking whether the findings are legitimate") {
		t.Error("expected review fix prompt to require validating prior findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not add code comments explaining your fixes") {
		t.Error("expected review fix prompt to forbid explanatory comments")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Verify that the issues are resolved") {
		t.Error("expected review fix prompt to require verification")
	}
	if !strings.Contains(ag.calls[0].Prompt, "possible nil dereference") {
		t.Error("expected review fix prompt to include previous findings")
	}
	if !strings.Contains(ag.calls[0].Prompt, `Return JSON with a single "summary" field`) {
		t.Error("expected fix prompt to request structured summary output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Keep the summary under 10 words") {
		t.Error("expected fix prompt to require a concise summary")
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if strings.Contains(ag.calls[1].Prompt, "feature code") {
		t.Error("expected review prompt to avoid embedding diff contents in fix mode")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(review): address review findings" {
		t.Fatalf("last commit message = %q", got)
	}
	if branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); branchSHA != sctx.Run.HeadSHA {
		t.Fatalf("branch SHA = %s, want %s", branchSHA, sctx.Run.HeadSHA)
	}
}

// --- Test step tests ---

func TestTestStep_Name(t *testing.T) {
	s := &TestStep{}
	if s.Name() != types.StepTest {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepTest)
	}
}

func TestTestStep_PassingCommand(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Test: "true"})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval for passing tests")
	}
	if len(ag.calls) != 0 {
		t.Error("expected no agent calls when test command passes")
	}
}

func TestTestStep_FailingCommand(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Test: "exit 1"})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed for failing tests")
	}
	if outcome.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", outcome.ExitCode)
	}

	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	if len(findings.Items) == 0 {
		t.Error("expected findings for failing tests")
	}
	if findings.Items[0].Severity != "error" {
		t.Errorf("severity = %s, want error", findings.Items[0].Severity)
	}
}

func TestTestStep_NoCommand_AgentDetects(t *testing.T) {
	dir := t.TempDir()

	findings := Findings{Items: nil, Summary: "all tests passed"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when agent reports passing tests")
	}
	if len(ag.calls) != 1 {
		t.Errorf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "If tests fail, determine whether the problem is") {
		t.Error("expected prompt to classify test failures")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do NOT run linters, formatters, or static analysis tools") {
		t.Error("expected prompt to forbid lint/format work in test step")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Writing and running new tests if coverage is insufficient") {
		t.Error("expected prompt to allow adding tests when needed")
	}
	if !strings.Contains(ag.calls[0].Prompt, "branch: refs/heads/feature") {
		t.Error("expected prompt to include branch metadata")
	}
	if !strings.Contains(ag.calls[0].Prompt, "empty findings array") {
		t.Error("expected prompt to instruct agent to return empty findings when tests pass")
	}
}

func TestTestStep_NoCommand_MalformedAgentOutput(t *testing.T) {
	dir := t.TempDir()

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "tests found some issues",
			}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Should fall back to text response when JSON is malformed
	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	if findings.Summary == "" {
		t.Error("expected fallback summary from text response when agent output is malformed JSON")
	}
}

func TestLintStep_NoCommand_MalformedAgentOutput(t *testing.T) {
	dir := t.TempDir()

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "lint found some issues",
			}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Should fall back to text response when JSON is malformed
	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	if findings.Summary == "" {
		t.Error("expected fallback summary from text response when agent output is malformed JSON")
	}
}

func TestTestStep_FixMode(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix + passing tests")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if !strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected test fix prompt to require minimal changes")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not refactor beyond what is needed") {
		t.Error("expected test fix prompt to forbid broader refactors")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Re-run the relevant tests") {
		t.Error("expected test fix prompt to require re-running tests")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do NOT run linters, formatters, or static analysis tools") {
		t.Error("expected test fix prompt to forbid lint/format work")
	}
	if !strings.Contains(ag.calls[0].Prompt, `Return JSON with a single "summary" field`) {
		t.Error("expected fix prompt to request structured summary output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Keep the summary under 10 words") {
		t.Error("expected fix prompt to require a concise summary")
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_FixMode_CommitsChanges(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint issues"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	sctx.Fixing = true

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix with passing lint")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if !strings.Contains(ag.calls[0].Prompt, `Return JSON with a single "summary" field`) {
		t.Error("expected fix prompt to request structured summary output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Keep the summary under 10 words") {
		t.Error("expected fix prompt to require a concise summary")
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fix lint issues" {
		t.Fatalf("last commit message = %q", got)
	}
}

// --- Lint step tests ---

func TestLintStep_Name(t *testing.T) {
	s := &LintStep{}
	if s.Name() != types.StepLint {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepLint)
	}
}

func TestLintStep_PassingCommand(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Lint: "true"})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval for clean lint")
	}
}

func TestLintStep_FailingCommand(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	lintCmd := "echo 'lint error'; exit 1"
	if runtime.GOOS == "windows" {
		lintCmd = "echo lint error & exit /b 1"
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Lint: lintCmd})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed for lint errors")
	}

	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	if len(findings.Items) == 0 {
		t.Error("expected findings for lint errors")
	}
	if findings.Items[0].Severity != "warning" {
		t.Errorf("severity = %s, want warning", findings.Items[0].Severity)
	}
}

func TestLintStep_NoCommand(t *testing.T) {
	dir := t.TempDir()

	findings := Findings{Items: nil, Summary: "no lint issues"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when agent reports no lint issues")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "Only lint or format the relevant changed files when possible") {
		t.Error("expected lint prompt to prefer changed-file scope")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not run tests or broader behavioral validation") {
		t.Error("expected lint prompt to avoid test work")
	}
	if !strings.Contains(ag.calls[0].Prompt, "branch: refs/heads/feature") {
		t.Error("expected lint prompt to include branch metadata")
	}
}

// --- Push step tests ---

func TestPushStep_Name(t *testing.T) {
	s := &PushStep{}
	if s.Name() != types.StepPush {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepPush)
	}
}

func TestPushStep_Success(t *testing.T) {
	// Set up a bare repo as "upstream"
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Create a regular repo and push initial commit to upstream
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("push should never need approval")
	}

	// Verify the push landed in upstream
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != headSHA {
		t.Errorf("upstream SHA = %s, want %s", upstreamSHA, headSHA)
	}
}

func TestPushStep_CommitsUncommittedChanges(t *testing.T) {
	// Set up upstream
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Create repo with initial push
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Add uncommitted changes (simulating agent fixes)
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("agent fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("push should never need approval")
	}

	// Verify fix.txt made it to upstream (committed and pushed)
	// Check by looking at the upstream's feature ref
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA == headSHA {
		t.Error("upstream should have a new commit with agent fixes, not the original headSHA")
	}
}

func TestPushStep_ShortBranch(t *testing.T) {
	// Set up upstream
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "mybranch")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("data"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "mybranch" // short branch name (no refs/heads/)

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify push with normalized ref
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/mybranch")
	if upstreamSHA != headSHA {
		t.Errorf("upstream SHA = %s, want %s", upstreamSHA, headSHA)
	}
}

func TestPushStep_NewBranchSkipsForceWithLease(t *testing.T) {
	// When the branch doesn't exist on upstream yet, push should use regular push
	// (not force-with-lease, which isn't needed for new branches).
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch but do NOT push it to upstream — it's a brand new branch
	gitCmd(t, dir, "checkout", "-b", "new-feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "new-feature"

	step := &PushStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("push should never need approval")
	}

	// Verify the new branch was created on upstream
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/new-feature")
	if upstreamSHA != headSHA {
		t.Errorf("upstream SHA = %s, want %s", upstreamSHA, headSHA)
	}
}

func TestPushStep_ForceWithLeaseUsesExplicitSHA(t *testing.T) {
	// When the branch already exists on upstream, push should use --force-with-lease
	// with the explicit upstream SHA (queried via ls-remote), not the bare form.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Push feature branch to upstream first (so it exists)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "v1.txt"), []byte("v1"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "v1")
	gitCmd(t, dir, "push", "origin", "feature")

	// Now amend the commit (simulating rebase/agent changes)
	os.WriteFile(filepath.Join(dir, "v2.txt"), []byte("v2"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "v2")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("push should never need approval")
	}

	// Verify force-push succeeded — upstream should have the new SHA
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != headSHA {
		t.Errorf("upstream SHA = %s, want %s", upstreamSHA, headSHA)
	}
}

func TestPushStep_RunsFormatCommandBeforeCommit(t *testing.T) {
	// When a format command is configured, the push step should run it
	// before committing, so agent changes are formatted before push.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Add uncommitted changes that need formatting
	os.WriteFile(filepath.Join(dir, "unformatted.txt"), []byte("  needs formatting  "), 0o644)

	// Use a format command that writes a marker file to prove it ran
	markerPath := filepath.Join(dir, ".format-ran")
	var formatCmd string
	if runtime.GOOS == "windows" {
		bat := filepath.Join(dir, "fmt.bat")
		os.WriteFile(bat, []byte(fmt.Sprintf("@copy nul \"%s\" >nul\r\n", markerPath)), 0o755)
		formatCmd = bat
	} else {
		formatCmd = fmt.Sprintf("touch %s", markerPath)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Format: formatCmd})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("push should never need approval")
	}

	// Verify the format command ran (marker file exists)
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Error("format command was not executed before commit")
	}
}

func TestPushStep_UpdatesLocalBranchRefAfterDetachedPush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "checkout", "--detach", originalHeadSHA)

	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("agent fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	newHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchSHA != newHeadSHA {
		t.Fatalf("branch ref SHA = %s, want %s", branchSHA, newHeadSHA)
	}
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != newHeadSHA {
		t.Fatalf("upstream SHA = %s, want %s", upstreamSHA, newHeadSHA)
	}
}

func TestPushStep_SkipsFormatWhenNotConfigured(t *testing.T) {
	// When no format command is configured, push step should not fail.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("data"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal("push should succeed without format command configured")
	}
}

func TestPushStep_FormatCommandFailureIsWarning(t *testing.T) {
	// If the format command fails, push should still proceed (log warning, don't fail).
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("data"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	var logMessages []string
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Format: "exit 1"})
	sctx.Repo.UpstreamURL = upstream
	sctx.Log = func(s string) { logMessages = append(logMessages, s) }

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal("push should succeed even if format command fails")
	}

	// Verify a warning was logged
	found := false
	for _, msg := range logMessages {
		if strings.Contains(msg, "format") && strings.Contains(msg, "warning") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about format failure in logs, got: %v", logMessages)
	}
}

func TestPushStep_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	// When push retries after a prior UpdateRunHeadSHA failure, there are no
	// uncommitted changes. The step must still reconcile the DB if HeadSHA is stale.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with a stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA // intentionally wrong
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// In-memory HeadSHA must match actual HEAD
	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}

	// DB record must also be updated
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
}

// --- PR step tests ---

func TestPRStep_Name(t *testing.T) {
	s := &PRStep{}
	if s.Name() != types.StepPR {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepPR)
	}
}

func TestPRStep_GhNotAvailable(t *testing.T) {
	// Verify the step skips gracefully when the required provider CLI is missing.
	if _, err := exec.LookPath("gh"); err == nil {
		// gh is available on this machine, so we can't force the missing-CLI path here.
		t.Skip("gh is available, skipping unavailable test")
	}

	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, "abc", "def", config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip when gh is unavailable, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
}

// prependPATH prepends binDir to PATH using the platform-specific separator.
func prependPATH(t *testing.T, binDir string) {
	t.Helper()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeCLIBinDir creates a temporary directory for fake CLI binaries.
// Unlike t.TempDir(), cleanup tolerates file locks from recently-executed
// binaries on Windows (which prevent immediate deletion).
func fakeCLIBinDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fakecli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return dir
}

// linkTestBinary creates a hard link (or copy) of the current test binary
// with the given name in binDir. On Windows, .exe is appended.
func linkTestBinary(t *testing.T, binDir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(binDir, name)
	if err := os.Link(exe, dst); err != nil {
		// Fallback to copy if hard link fails (cross-device, etc.)
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeGH creates a mock gh binary in a temp dir and returns the dir (for PATH prepending).
// The binary records all invocations to a log file and responds based on subcommand.
func fakeGH(t *testing.T, prViewURL string) (binDir string, logFile string) {
	t.Helper()
	binDir = fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")
	t.Setenv("FAKE_CLI_MODE", "gh")
	t.Setenv("FAKE_CLI_LOG", logFile)
	t.Setenv("FAKE_CLI_PR_URL", prViewURL)
	return binDir, logFile
}

func TestPRStep_UpdatesExistingPR(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	prependPATH(t, binDir)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr edit was called to update the PR body
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr edit") {
		t.Errorf("expected gh pr edit to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--body") {
		t.Errorf("expected --body flag in gh pr edit, got:\n%s", ghLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/42" {
		t.Errorf("PR URL = %v, want https://github.com/test/repo/pull/42", run.PRURL)
	}
}

func TestPRStep_ZeroBaseSHA(t *testing.T) {
	// New branch scenario: baseSHA is all-zeros, commit log should still work
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	binDir, logFile := fakeGH(t, "")
	prependPATH(t, binDir)

	ag := &mockAgent{name: "test"}
	zeroSHA := "0000000000000000000000000000000000000000"
	sctx := newTestContextWithDBRecords(t, ag, dir, zeroSHA, headSHA, config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called (not blocked by zero SHA)
	logData, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logData), "pr create") {
		t.Errorf("expected gh pr create, got:\n%s", logData)
	}
}

func TestPRStep_CreatesNewPR(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	// No existing PR — pr view returns exit 1
	binDir, logFile := fakeGH(t, "")
	prependPATH(t, binDir)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("pr step should never need approval")
	}

	// Verify gh pr create was called
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr create") {
		t.Errorf("expected gh pr create to be called, got:\n%s", ghLog)
	}

	// Verify PR URL was stored
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://github.com/test/repo/pull/99" {
		t.Errorf("PR URL = %v, want https://github.com/test/repo/pull/99", run.PRURL)
	}
}

func TestPRStep_UsesAgentGeneratedTitleAndBody(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir, logFile := fakeGH(t, "")
	prependPATH(t, binDir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"Improve pipeline header UX","body":"## Summary\n\n- keep branch status readable\n- fix footer truncation"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "--title Improve pipeline header UX") {
		t.Fatalf("expected generated PR title in gh call, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "keep branch status readable") {
		t.Fatalf("expected generated PR body in gh call, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--title feature") {
		t.Fatalf("expected PR title to avoid raw branch name, got:\n%s", ghLog)
	}
}

func fakeGlab(t *testing.T, mrViewJSON string) (binDir string, logFile string) {
	t.Helper()
	binDir = fakeCLIBinDir(t)
	logFile = filepath.Join(t.TempDir(), "glab.log")
	linkTestBinary(t, binDir, "glab")
	t.Setenv("FAKE_CLI_MODE", "glab")
	t.Setenv("FAKE_CLI_LOG", logFile)
	t.Setenv("FAKE_CLI_MR_VIEW_JSON", mrViewJSON)
	return binDir, logFile
}

func TestPRStep_GitLabCreatesNewMR(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir, logFile := fakeGlab(t, "")
	prependPATH(t, binDir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"Improve gitlab flow","body":"## Summary\n\n- add gitlab support\n\n## Testing\n\n- go test ./..."}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "mr create") {
		t.Fatalf("expected glab mr create to be called, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--title Improve gitlab flow") {
		t.Fatalf("expected generated title in glab call, got:\n%s", ghLog)
	}
}

func TestPRStep_SkipsWhenProviderCLIUnavailable(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected skip instead of failure, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when PR step skips")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL != nil {
		t.Fatalf("expected no PR URL when provider CLI unavailable, got %q", *run.PRURL)
	}
}

func TestPRStep_ExistingBranchUsesMergeBaseCommitLog(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "first feature commit")
	oldRemoteSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "second feature commit")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	binDir, logFile := fakeGH(t, "")
	prependPATH(t, binDir)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, oldRemoteSHA, headSHA, config.Commands{})

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "first feature commit") {
		t.Errorf("expected PR body to include first feature commit, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "second feature commit") {
		t.Errorf("expected PR body to include second feature commit, got:\n%s", ghLog)
	}
}

// newTestContextWithDBRecords is like newTestContext but also inserts
// repo and run records into the database so GetRun works after updates.
func newTestContextWithDBRecords(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, workDir, baseSHA, headSHA, cmds)

	// Insert repo + run records so DB queries work
	repo, err := sctx.DB.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	sctx.Repo = repo
	return sctx
}

// --- Findings JSON round-trip ---

func TestFindingsJSON(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{Severity: "error", File: "main.go", Line: 42, Description: "null deref"},
			{Severity: "info", Description: "consider renaming"},
		},
		Summary: "2 findings",
	}

	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}

	var f2 Findings
	if err := json.Unmarshal(data, &f2); err != nil {
		t.Fatal(err)
	}

	if len(f2.Items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(f2.Items))
	}
	if f2.Items[0].File != "main.go" {
		t.Errorf("file = %s, want main.go", f2.Items[0].File)
	}
	if f2.Items[1].Line != 0 {
		t.Errorf("line = %d, want 0 (omitted)", f2.Items[1].Line)
	}
	if f2.Summary != "2 findings" {
		t.Errorf("summary = %s, want '2 findings'", f2.Summary)
	}
}

// --- Helper function unit tests ---

func TestNormalizedBranchRef(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"feature", "refs/heads/feature"},
		{"my/branch", "refs/heads/my/branch"},
		{"refs/heads/feature", "refs/heads/feature"},
		{"refs/tags/v1", "refs/tags/v1"},
	}
	for _, tc := range tests {
		if got := normalizedBranchRef(tc.input); got != tc.want {
			t.Errorf("normalizedBranchRef(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDeterministicFixCommitMessage(t *testing.T) {
	tests := []struct {
		step    types.StepName
		summary string
		want    string
	}{
		{types.StepReview, "address nil dereference", "no-mistakes(review): address nil dereference"},
		{types.StepTest, "", "no-mistakes(test): apply fixes"},
		{types.StepLint, "fix formatting", "no-mistakes(lint): fix formatting"},
	}
	for _, tc := range tests {
		if got := deterministicFixCommitMessage(tc.step, tc.summary); got != tc.want {
			t.Errorf("deterministicFixCommitMessage(%q, %q) = %q, want %q", tc.step, tc.summary, got, tc.want)
		}
	}
}

func TestExtractCommitSummary(t *testing.T) {
	tests := []struct {
		name    string
		result  *agent.Result
		want    string
		wantErr bool
	}{
		{
			name:   "valid summary",
			result: &agent.Result{Output: json.RawMessage(`{"summary":"fix nil pointer"}`)},
			want:   "fix nil pointer",
		},
		{
			name:   "trims punctuation and whitespace",
			result: &agent.Result{Output: json.RawMessage(`{"summary":"  'fix lint issues.'  "}`)},
			want:   "fix lint issues",
		},
		{
			name:    "nil output",
			result:  &agent.Result{},
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			result:  &agent.Result{Output: json.RawMessage(`not json`)},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractCommitSummary(tc.result)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("extractCommitSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCommitAgentFixes_NoChanges(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	originalHeadSHA := sctx.Run.HeadSHA

	err := commitAgentFixes(sctx, types.StepReview, "should not commit", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if sctx.Run.HeadSHA != originalHeadSHA {
		t.Errorf("HeadSHA changed unexpectedly: %s -> %s", originalHeadSHA, sctx.Run.HeadSHA)
	}
}

func TestCommitAgentFixes_UsesFallbackSummary(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	os.WriteFile(filepath.Join(dir, "agent-change.txt"), []byte("change"), 0o644)
	err := commitAgentFixes(sctx, types.StepLint, "", "fallback lint fix")
	if err != nil {
		t.Fatal(err)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fallback lint fix" {
		t.Errorf("commit message = %q, want fallback-based message", got)
	}
}

// --- Babysit step tests ---

func TestBabysitStep_Name(t *testing.T) {
	s := &BabysitStep{}
	if s.Name() != types.StepBabysit {
		t.Errorf("Name() = %s, want %s", s.Name(), types.StepBabysit)
	}
}

func TestExtractPRNumber(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"standard", "https://github.com/owner/repo/pull/42", "42", false},
		{"trailing slash", "https://github.com/owner/repo/pull/42/", "42", false},
		{"just number", "123", "123", false},
		{"empty", "", "", true},
		{"non numeric", "https://github.com/owner/repo/pull/abc", "", true},
		{"path segment not number", "https://github.com/owner/repo/pull/42/files", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractPRNumber(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractPRNumber(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("extractPRNumber(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestPollInterval(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"start", 0, 30 * time.Second},
		{"3 minutes", 3 * time.Minute, 30 * time.Second},
		{"5 minutes", 5 * time.Minute, 60 * time.Second},
		{"10 minutes", 10 * time.Minute, 60 * time.Second},
		{"15 minutes", 15 * time.Minute, 120 * time.Second},
		{"1 hour", time.Hour, 120 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pollInterval(tt.elapsed)
			if got != tt.want {
				t.Errorf("pollInterval(%v) = %v, want %v", tt.elapsed, got, tt.want)
			}
		})
	}
}

func TestHasFailingChecks(t *testing.T) {
	tests := []struct {
		name   string
		checks []ciCheck
		want   bool
	}{
		{"empty", nil, false},
		{"all passing", []ciCheck{{Name: "build", Conclusion: "success"}}, false},
		{"failure", []ciCheck{{Name: "build", Conclusion: "failure"}}, true},
		{"action required", []ciCheck{{Name: "lint", Conclusion: "action_required"}}, true},
		{"mixed", []ciCheck{
			{Name: "build", Conclusion: "success"},
			{Name: "test", Conclusion: "failure"},
		}, true},
		{"neutral", []ciCheck{{Name: "build", Conclusion: "neutral"}}, false},
		{"pending", []ciCheck{{Name: "build", Status: "QUEUED", Conclusion: ""}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasFailingChecks(tt.checks)
			if got != tt.want {
				t.Errorf("hasFailingChecks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasPendingChecks(t *testing.T) {
	tests := []struct {
		name   string
		checks []ciCheck
		want   bool
	}{
		{"empty", nil, false},
		{"all completed", []ciCheck{{Name: "build", Status: "COMPLETED", Conclusion: "success"}}, false},
		{"queued", []ciCheck{{Name: "build", Status: "QUEUED", Conclusion: ""}}, true},
		{"in progress", []ciCheck{{Name: "build", Status: "IN_PROGRESS", Conclusion: ""}}, true},
		{"mixed", []ciCheck{
			{Name: "build", Status: "COMPLETED", Conclusion: "success"},
			{Name: "test", Status: "IN_PROGRESS", Conclusion: ""},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasPendingChecks(tt.checks)
			if got != tt.want {
				t.Errorf("hasPendingChecks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFailingCheckNames(t *testing.T) {
	checks := []ciCheck{
		{Name: "build", Conclusion: "success"},
		{Name: "test", Conclusion: "failure"},
		{Name: "lint", Conclusion: "action_required"},
		{Name: "deploy", Conclusion: "neutral"},
	}
	got := failingCheckNames(checks)
	if len(got) != 2 {
		t.Fatalf("failingCheckNames() returned %d names, want 2", len(got))
	}
	if got[0] != "test" || got[1] != "lint" {
		t.Errorf("failingCheckNames() = %v, want [test, lint]", got)
	}
}

func TestCommentsToFindings(t *testing.T) {
	comments := []prComment{
		{ID: "1", Body: "Please fix the null check"},
		{ID: "2", Body: "LGTM"},
	}
	comments[0].Author.Login = "alice"
	comments[1].Author.Login = "bob"

	findings := commentsToFindings(comments)
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings.Items))
	}
	if findings.Items[0].Severity != "info" {
		t.Errorf("severity = %s, want info", findings.Items[0].Severity)
	}
	if !strings.Contains(findings.Items[0].Description, "@alice") {
		t.Error("expected finding to contain @alice")
	}
	if !strings.Contains(findings.Items[0].Description, "null check") {
		t.Error("expected finding to contain comment body")
	}
	if findings.Summary != "2 PR comment(s) to review" {
		t.Errorf("summary = %q, want '2 PR comment(s) to review'", findings.Summary)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"long", "hello world", 8, "hello..."},
		{"very short max", "hello", 3, "hel"},
		{"empty", "", 5, ""},
		// Multi-byte rune handling: truncate should count runes, not bytes
		{"multibyte exact", "日本語AB", 5, "日本語AB"},
		{"multibyte truncated", "日本語ABCD", 5, "日本..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestBabysitStep_NoPRURL(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Run.PRURL = nil

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval for missing PR URL")
	}
}

func TestBabysitStep_NonGitHubSkips(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected babysit skip for non-GitHub provider")
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "skipping babysit") {
		t.Fatalf("expected skip log, got: %v", logs)
	}
}

func TestBabysitStep_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/1"
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	sctx.Ctx = ctx

	step := &BabysitStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestBabysitStep_TimeoutDoesNotSleepPastDeadline(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir := fakeBabysitGH(t, "OPEN", "[]", "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 2 * time.Second

	step := &BabysitStep{}
	started := time.Now()
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when timeout expires")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("babysit step exceeded timeout budget: %v", elapsed)
	}
}

func TestAllStepsIncludesBabysit(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 7 {
		t.Fatalf("AllSteps() returned %d steps, want 7", len(steps))
	}
	if steps[6].Name() != types.StepBabysit {
		t.Errorf("last step = %s, want %s", steps[6].Name(), types.StepBabysit)
	}
}

func TestBabysitStep_CommitAndPush(t *testing.T) {
	// Set up upstream bare repo
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Create working repo
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Add uncommitted changes
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("babysit fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	step := &BabysitStep{}
	err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the commit and push happened
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA == headSHA {
		t.Error("upstream should have a new commit with babysit fixes")
	}
}

func TestBabysitStep_CommitAndPush_NoChanges(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "dummy"
	sctx.Run.Branch = "refs/heads/feature"

	step := &BabysitStep{}
	err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	// No error expected — just a no-op
}

func TestBabysitStep_CommitAndPush_NoChanges_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	step := &BabysitStep{}
	err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
}

func TestBabysitStep_CommitAndPush_UpdatesLocalBranchRefAfterDetachedPush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "checkout", "--detach", originalHeadSHA)
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("babysit fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	step := &BabysitStep{}
	err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	newHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	branchSHA := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchSHA != newHeadSHA {
		t.Fatalf("branch ref SHA = %s, want %s", branchSHA, newHeadSHA)
	}
	upstreamSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if upstreamSHA != newHeadSHA {
		t.Fatalf("upstream SHA = %s, want %s", upstreamSHA, newHeadSHA)
	}
}

func TestCICheckJSON(t *testing.T) {
	input := `[{"name":"build","status":"COMPLETED","conclusion":"success"},{"name":"test","status":"COMPLETED","conclusion":"failure"}]`
	var checks []ciCheck
	if err := json.Unmarshal([]byte(input), &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}
	if !hasFailingChecks(checks) {
		t.Error("expected failing checks")
	}
	names := failingCheckNames(checks)
	if len(names) != 1 || names[0] != "test" {
		t.Errorf("failingCheckNames = %v, want [test]", names)
	}
}

func TestPRCommentJSON(t *testing.T) {
	input := `[{"id":"IC_123","author":{"login":"reviewer"},"body":"Please fix this","createdAt":"2026-01-01T00:00:00Z","url":"https://github.com/test/repo/pull/1#comment-123"}]`
	var comments []prComment
	if err := json.Unmarshal([]byte(input), &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Author.Login != "reviewer" {
		t.Errorf("author = %s, want reviewer", comments[0].Author.Login)
	}
	if comments[0].Body != "Please fix this" {
		t.Errorf("body = %s, want 'Please fix this'", comments[0].Body)
	}
}

// --- isTestFile tests ---

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Go
		{"foo_test.go", true},
		{"internal/pkg/bar_test.go", true},
		// Python
		{"test_foo.py", true},
		{"foo_test.py", true},
		{"tests/test_bar.py", true},
		// JavaScript/TypeScript
		{"app.test.js", true},
		{"app.spec.ts", true},
		{"src/component.test.tsx", true},
		{"src/component.spec.jsx", true},
		// Java
		{"FooTest.java", true},
		{"BarTests.java", true},
		// Rust
		{"foo_test.rs", true},
		// Ruby
		{"test_foo.rb", true},
		// Non-test files
		{"main.go", false},
		{"test.txt", false},
		{"testing.go", false},
		{"contest.py", false},
		{"foo.js", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isTestFile(tt.path); got != tt.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestDetectNewTestFiles(t *testing.T) {
	dir, _, _ := setupGitRepo(t)

	// Add an untracked test file
	os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package main\n"), 0o644)
	// Add a non-test untracked file
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# readme\n"), 0o644)

	files := detectNewTestFiles(context.Background(), dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 test file, got %d: %v", len(files), files)
	}
	if files[0] != "new_test.go" {
		t.Errorf("expected new_test.go, got %s", files[0])
	}
}

func TestDetectNewTestFiles_StagedFiles(t *testing.T) {
	dir, _, _ := setupGitRepo(t)

	// Add a staged test file
	os.WriteFile(filepath.Join(dir, "foo_test.py"), []byte("def test_foo(): pass\n"), 0o644)
	gitCmd(t, dir, "add", "foo_test.py")

	files := detectNewTestFiles(context.Background(), dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 test file, got %d: %v", len(files), files)
	}
	if files[0] != "foo_test.py" {
		t.Errorf("expected foo_test.py, got %s", files[0])
	}
}

func TestDetectNewTestFiles_NoNewFiles(t *testing.T) {
	dir, _, _ := setupGitRepo(t)

	files := detectNewTestFiles(context.Background(), dir)
	if len(files) != 0 {
		t.Errorf("expected no test files, got %v", files)
	}
}

func TestTestStep_AgentWritesNewTests_NeedsApproval(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{Items: nil, Summary: "all tests passed"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Simulate agent creating a new test file
			os.WriteFile(filepath.Join(dir, "agent_test.go"), []byte("package main\n"), 0o644)
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent writes new test files")
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "agent_test.go") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning agent_test.go, got findings: %+v", f.Items)
	}
}

func TestTestStep_FixMode_AgentWritesNewTests_NeedsApproval(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			// Simulate agent creating a new test file during fix
			os.WriteFile(filepath.Join(dir, "fix_test.go"), []byte("package main\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"add regression test"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent writes new test files in fix mode")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call in fix mode, got %d", callCount)
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "fix_test.go") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning fix_test.go, got findings: %+v", f.Items)
	}
}

// --- ignore patterns tests ---

func TestMatchIgnorePattern(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		// No-slash patterns match against basename
		{"pkg/foo.generated.go", "*.generated.go", true},
		{"foo.generated.go", "*.generated.go", true},
		{"deep/nested/bar.generated.go", "*.generated.go", true},
		{"foo.go", "*.generated.go", false},

		// Directory wildcard patterns
		{"vendor/pkg/foo.go", "vendor/**", true},
		{"vendor/foo.go", "vendor/**", true},
		{"vendor", "vendor/**", true},
		{"myvendor/foo.go", "vendor/**", false},
		{"src/vendor/foo.go", "vendor/**", false},

		// Full path patterns
		{"docs/README.md", "docs/*.md", true},
		{"README.md", "docs/*.md", false},

		// No match
		{"main.go", "*.generated.go", false},
		{"internal/app.go", "vendor/**", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.path, tt.pattern), func(t *testing.T) {
			got := matchIgnorePattern(tt.path, tt.pattern)
			if got != tt.want {
				t.Errorf("matchIgnorePattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestExtractDiffPath(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"diff --git a/foo.go b/foo.go", "foo.go"},
		{"diff --git a/pkg/bar.go b/pkg/bar.go", "pkg/bar.go"},
		{"diff --git a/vendor/lib.go b/vendor/lib.go", "vendor/lib.go"},
		{"not a diff line", ""},
		// Path containing " b/" should not be split incorrectly
		{"diff --git a/a b/c.go b/a b/c.go", "a b/c.go"},
		{"diff --git a/x b/y b/z.go b/x b/y b/z.go", "x b/y b/z.go"},
	}
	for _, tt := range tests {
		got := extractDiffPath(tt.line)
		if got != tt.want {
			t.Errorf("extractDiffPath(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestFilterDiff_Empty(t *testing.T) {
	// No patterns → unchanged
	diff := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n+line\n"
	got := filterDiff(diff, nil)
	if got != diff {
		t.Errorf("expected diff unchanged with nil patterns")
	}

	// Empty diff → empty
	got = filterDiff("", []string{"*.go"})
	if got != "" {
		t.Errorf("expected empty output for empty diff")
	}
}

func TestFilterDiff_SingleFile(t *testing.T) {
	diff := "diff --git a/foo.generated.go b/foo.generated.go\n--- a/foo.generated.go\n+++ b/foo.generated.go\n@@ -0,0 +1 @@\n+generated\n"
	got := filterDiff(diff, []string{"*.generated.go"})
	// All lines should be filtered
	if strings.Contains(got, "generated") {
		t.Errorf("expected generated file to be filtered out, got: %q", got)
	}
}

func TestFilterDiff_MultipleFiles(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1 +1 @@",
		"+package main",
		"diff --git a/vendor/lib.go b/vendor/lib.go",
		"--- a/vendor/lib.go",
		"+++ b/vendor/lib.go",
		"@@ -0,0 +1 @@",
		"+vendored",
		"diff --git a/internal/app.go b/internal/app.go",
		"--- a/internal/app.go",
		"+++ b/internal/app.go",
		"@@ -1 +1 @@",
		"+app code",
	}, "\n")

	got := filterDiff(diff, []string{"vendor/**"})

	// main.go should be kept
	if !strings.Contains(got, "main.go") {
		t.Error("expected main.go to remain in diff")
	}
	// vendor/lib.go should be filtered
	if strings.Contains(got, "vendor/lib.go") {
		t.Error("expected vendor/lib.go to be filtered out")
	}
	// internal/app.go should be kept
	if !strings.Contains(got, "internal/app.go") {
		t.Error("expected internal/app.go to remain in diff")
	}
}

func TestFilterDiff_MultiplePatterns(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"+++ b/main.go",
		"+keep",
		"diff --git a/generated.pb.go b/generated.pb.go",
		"+++ b/generated.pb.go",
		"+proto",
		"diff --git a/vendor/dep.go b/vendor/dep.go",
		"+++ b/vendor/dep.go",
		"+dep",
	}, "\n")

	got := filterDiff(diff, []string{"*.pb.go", "vendor/**"})

	if !strings.Contains(got, "main.go") {
		t.Error("expected main.go to remain")
	}
	if strings.Contains(got, "generated.pb.go") {
		t.Error("expected generated.pb.go to be filtered")
	}
	if strings.Contains(got, "vendor/dep.go") {
		t.Error("expected vendor/dep.go to be filtered")
	}
}

func TestReviewFindingsSchema_ValidJSON(t *testing.T) {
	if !json.Valid(reviewFindingsSchema) {
		t.Errorf("reviewFindingsSchema is not valid JSON: %s", string(reviewFindingsSchema))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(reviewFindingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	required, ok := parsed["required"].([]interface{})
	if !ok {
		t.Fatal("expected 'required' array in schema")
	}
	want := map[string]bool{"findings": false, "risk_level": false, "risk_rationale": false}
	for _, r := range required {
		s, _ := r.(string)
		want[s] = true
	}
	for field, found := range want {
		if !found {
			t.Errorf("missing required field %q in schema", field)
		}
	}
}

func TestReviewStep_IgnorePatterns(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Add a generated file to the feature branch
	os.WriteFile(filepath.Join(dir, "schema.generated.go"), []byte("package gen\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add generated file")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			findings := Findings{Summary: "looks good", Items: nil}
			out, _ := json.Marshal(findings)
			return &agent.Result{Output: out}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.IgnorePatterns = []string{"*.generated.go"}

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(capturedPrompt, "*.generated.go") {
		t.Error("expected prompt to include ignore patterns")
	}
	if !strings.Contains(capturedPrompt, headSHA) {
		t.Error("expected prompt to include head SHA metadata")
	}
	if strings.Contains(capturedPrompt, "schema.generated.go") {
		t.Error("expected prompt to avoid embedding changed file names")
	}
	if strings.Contains(capturedPrompt, "feature.txt") {
		t.Error("expected prompt to avoid embedding changed file names")
	}
}

func TestReviewStep_IgnorePatternsFilterAllFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a repo where the only change is a generated file
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "schema.generated.go"), []byte("package gen\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add generated")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.IgnorePatterns = []string{"*.generated.go"}

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// When all files are filtered, should complete with no approval needed
	if outcome.NeedsApproval {
		t.Error("expected no approval when all changes are in ignored files")
	}
	// Agent should not have been called
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent calls when diff is empty after filtering, got %d", len(ag.calls))
	}
}

// --- Fix prompt with previous findings tests ---

func TestReviewStep_FixMode_IncludesPreviousFindings(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	previousFindings := `{"items":[{"severity":"error","file":"main.go","line":42,"description":"nil pointer dereference"}],"summary":"1 error found"}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				// Fix call — verify prompt includes previous findings
				if !strings.Contains(opts.Prompt, "Previous review findings to address") {
					t.Error("fix prompt should contain 'Previous review findings to address'")
				}
				if !strings.Contains(opts.Prompt, "nil pointer dereference") {
					t.Error("fix prompt should contain the specific finding description")
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"address review findings"}`)}, nil
			}
			// Review call
			findings := Findings{Summary: "all clear"}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed after fix")
	}
	if callCount != 2 {
		t.Errorf("expected 2 agent calls (fix + review), got %d", callCount)
	}
}

func TestTestStep_FixMode_IncludesPreviousFindings(t *testing.T) {
	dir := t.TempDir()

	previousFindings := `{"items":[{"severity":"error","description":"tests failed with exit code 1"}],"summary":"FAIL: TestFoo expected 42 got 0"}`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Fix call — verify prompt includes previous findings
			if !strings.Contains(opts.Prompt, "Previous test findings to address") {
				t.Error("fix prompt should contain 'Previous test findings to address'")
			}
			if !strings.Contains(opts.Prompt, "FAIL: TestFoo expected 42 got 0") {
				t.Error("fix prompt should contain the specific test output")
			}
			if !strings.Contains(opts.Prompt, "Make the minimal change needed") {
				t.Error("fix prompt should require minimal changes")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures"}`)}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Test: "true"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed after fix with passing tests")
	}
}

func TestLintStep_FixMode_IncludesPreviousFindings(t *testing.T) {
	dir := t.TempDir()

	previousFindings := `{"items":[{"severity":"warning","description":"linter found issues (exit code 1)"}],"summary":"main.go:10: unused variable x"}`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Fix call — verify prompt includes previous findings
			if !strings.Contains(opts.Prompt, "Previous lint findings to address") {
				t.Error("fix prompt should contain 'Previous lint findings to address'")
			}
			if !strings.Contains(opts.Prompt, "unused variable x") {
				t.Error("fix prompt should contain the specific lint output")
			}
			if !strings.Contains(opts.Prompt, "Make the minimal change needed") {
				t.Error("fix prompt should require minimal changes")
			}
			if !strings.Contains(opts.Prompt, "Re-run the relevant lint or format commands") {
				t.Error("fix prompt should require lint verification")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint issues"}`)}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Lint: "true"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed after fix with passing lint")
	}
}

func TestReviewStep_FixMode_RequiresPreviousFindings(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("agent should not be called when fix mode has no previous findings")
			return nil, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	// PreviousFindings left empty intentionally

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error when fix mode has no previous findings")
	}
	if !strings.Contains(err.Error(), "previous review findings") {
		t.Fatalf("error = %q, want to mention previous review findings", err)
	}
}

// --- babysit Execute tests ---

// fakeBabysitGH creates a fake gh binary that responds to babysit-related
// commands (pr view --json state, pr checks --json, pr view --json comments).
func fakeBabysitGH(t *testing.T, state, checksJSON, commentsJSON string) string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	t.Setenv("FAKE_CLI_MODE", "babysit-gh")
	t.Setenv("FAKE_CLI_STATE", state)
	t.Setenv("FAKE_CLI_CHECKS", checksJSON)
	t.Setenv("FAKE_CLI_COMMENTS", commentsJSON)
	return binDir
}

func fakeBabysitGHNoChecks(t *testing.T) string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	t.Setenv("FAKE_CLI_MODE", "babysit-gh-nochecks")
	return binDir
}

func TestBabysitStep_PRMergedExitsEarly(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir := fakeBabysitGH(t, "MERGED", "[]", "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for merged PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "merged") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'merged' in logs, got: %v", logs)
	}
}

func TestBabysitStep_PRClosedExitsEarly(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir := fakeBabysitGH(t, "CLOSED", "[]", "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for closed PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "closed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'closed' in logs, got: %v", logs)
	}
}

func TestBabysitStep_GetCIChecksNoChecksReported(t *testing.T) {
	binDir := fakeBabysitGHNoChecks(t)
	prependPATH(t, binDir)

	step := &BabysitStep{}
	checks, err := step.getCIChecks(context.Background(), t.TempDir(), "42")
	if err != nil {
		t.Fatalf("expected no error when gh reports no checks, got: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected no checks, got: %#v", checks)
	}
}

func TestBabysitStep_CIFailureAutoFix(t *testing.T) {
	// Set up upstream bare repo for push
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"build","status":"COMPLETED","conclusion":"success"},{"name":"test","status":"COMPLETED","conclusion":"failure"}]`
	binDir := fakeBabysitGH(t, "OPEN", checksJSON, "[]")
	prependPATH(t, binDir)

	agentCalled := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			// Agent "fixes" CI by creating a file
			os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.BabysitTimeout = 30 * time.Second

	// Use a context with short timeout: after auto-fix completes, the poll
	// sleep (30s) will be interrupted by context deadline, exiting the loop.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	_, err := step.Execute(sctx)
	// Expect context deadline exceeded (interrupted during poll sleep after auto-fix)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if !agentCalled {
		t.Error("expected agent to be called for CI auto-fix")
	}

	// Verify agent was called with CI failure context
	if len(ag.calls) == 0 {
		t.Fatal("expected agent call")
	}
	if !strings.Contains(ag.calls[0].Prompt, "test") {
		t.Errorf("expected failing check name in prompt, got: %s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected CI fix prompt to require minimal changes")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not refactor beyond what is needed") {
		t.Error("expected CI fix prompt to forbid broader refactors")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Verify the fix by running the most relevant commands locally") {
		t.Error("expected CI fix prompt to require verification")
	}

	foundAutoFix := false
	for _, l := range logs {
		if strings.Contains(l, "CI failures detected") {
			foundAutoFix = true
			break
		}
	}
	if !foundAutoFix {
		t.Errorf("expected CI failure detection in logs, got: %v", logs)
	}
}

func TestBabysitStep_NewCommentsPausesForApproval(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	commentsJSON := `[{"id":"IC_100","author":{"login":"reviewer"},"body":"Please fix the naming","createdAt":"2026-01-01T00:00:00Z","url":"https://github.com/test/repo/pull/42#comment-100"}]`
	binDir := fakeBabysitGH(t, "OPEN", "[]", commentsJSON)
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed for new PR comments")
	}
	if outcome.Findings == "" {
		t.Error("expected findings with comment details")
	}

	// Verify findings contain the comment
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings.Items))
	}
	if findings.Items[0].ID != "IC_100" {
		t.Fatalf("expected comment ID to be used as finding ID, got %q", findings.Items[0].ID)
	}
	if !strings.Contains(findings.Items[0].Description, "reviewer") {
		t.Errorf("expected reviewer in finding, got: %s", findings.Items[0].Description)
	}
	if !strings.Contains(findings.Items[0].Description, "naming") {
		t.Errorf("expected comment body in finding, got: %s", findings.Items[0].Description)
	}

	foundCommentLog := false
	for _, l := range logs {
		if strings.Contains(l, "new PR comment") {
			foundCommentLog = true
			break
		}
	}
	if !foundCommentLog {
		t.Errorf("expected new comment log, got: %v", logs)
	}
}

func TestBabysitStep_AllChecksPassingExitsCleanly(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	binDir := fakeBabysitGH(t, "OPEN", checksJSON, "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when CI checks are already passing")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected clean-exit CI log, got: %v", logs)
	}
}

func TestBabysitStep_EmptyChecksWaitsDuringGracePeriod(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Fake gh returns OPEN state, empty checks, no comments
	binDir := fakeBabysitGH(t, "OPEN", "[]", "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &BabysitStep{checksGracePeriod: 200 * time.Millisecond, pollIntervalOverride: 10 * time.Millisecond}
	started := time.Now()
	outcome, err := step.Execute(sctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed")
	}
	// Must have waited at least the grace period before exiting
	if elapsed < 200*time.Millisecond {
		t.Errorf("babysit exited in %v, expected to wait at least 200ms grace period", elapsed)
	}
	// Should exit via grace period expiry, not babysit timeout
	for _, l := range logs {
		if strings.Contains(l, "babysit timeout reached") {
			t.Fatal("expected exit via grace period expiry, not babysit timeout")
		}
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "babysit complete") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'babysit complete' log, got: %v", logs)
	}
}

func TestBabysitStep_NonEmptyPassingChecksExitImmediately(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	binDir := fakeBabysitGH(t, "OPEN", checksJSON, "[]")
	prependPATH(t, binDir)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.BabysitTimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	// Even with a long grace period, non-empty passing checks should exit immediately
	step := &BabysitStep{checksGracePeriod: 10 * time.Second}
	started := time.Now()
	outcome, err := step.Execute(sctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed")
	}
	if elapsed > 5*time.Second {
		t.Errorf("non-empty passing checks should exit quickly, took %v", elapsed)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'all CI checks passed' log, got: %v", logs)
	}
}

func TestBabysitStep_AddressCommentsInFixMode_OnlySelectedComments(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	commentsJSON := `[
		{"id":"IC_200","author":{"login":"alice"},"body":"Rename this function","createdAt":"2026-01-01T00:00:00Z","url":"https://github.com/test/repo/pull/42#comment-200"},
		{"id":"IC_201","author":{"login":"bob"},"body":"Add more tests","createdAt":"2026-01-01T00:00:01Z","url":"https://github.com/test/repo/pull/42#comment-201"}
	]`
	passingChecks := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	binDir := fakeBabysitGH(t, "OPEN", passingChecks, commentsJSON)
	prependPATH(t, binDir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(opts.CWD, "comment-fix.txt"), []byte("fixed"), 0o644)
			if !strings.Contains(opts.Prompt, "Rename this function") {
				t.Fatalf("expected selected comment in agent prompt, got: %s", opts.Prompt)
			}
			if strings.Contains(opts.Prompt, "Add more tests") {
				t.Fatalf("did not expect unselected comment in agent prompt, got: %s", opts.Prompt)
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"IC_200","severity":"info","description":"@alice: Rename this function"}],"summary":"1 PR comment(s) to review"}`
	sctx.Config.BabysitTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	step := &BabysitStep{
		seenComments: map[string]bool{"IC_200": true, "IC_201": true},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected clean exit after addressing comments, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after selected comments are addressed")
	}
}

func TestBabysitStep_AddressCommentsInFixMode(t *testing.T) {
	// Set up upstream bare repo for push
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	commentsJSON := `[{"id":"IC_200","author":{"login":"alice"},"body":"Rename this function","createdAt":"2026-01-01T00:00:00Z","url":"https://github.com/test/repo/pull/42#comment-200"}]`
	passingChecks := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	binDir := fakeBabysitGH(t, "OPEN", passingChecks, commentsJSON)
	prependPATH(t, binDir)

	agentCalled := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			// Agent fixes by creating a file
			os.WriteFile(filepath.Join(opts.CWD, "comment-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Fixing = true
	sctx.Config.BabysitTimeout = 30 * time.Second

	// Use a context with short timeout: after addressing comments and entering
	// the poll loop, the sleep will be interrupted by context deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	// Pre-populate seenComments to simulate comments from previous Execute
	step := &BabysitStep{
		seenComments: map[string]bool{"IC_200": true},
	}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected clean exit after addressing comments, got: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after addressed comments and clean CI")
	}
	if !agentCalled {
		t.Error("expected agent to be called for comment addressing")
	}

	// Verify agent prompt includes the comment text
	if len(ag.calls) == 0 {
		t.Fatal("expected agent call")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Rename this function") {
		t.Errorf("expected comment body in agent prompt, got: %s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected comment-fix prompt to require minimal changes")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not refactor beyond what is needed") {
		t.Error("expected comment-fix prompt to forbid broader refactors")
	}
	if !strings.Contains(ag.calls[0].Prompt, "Do not add comments explaining your fixes") {
		t.Error("expected comment-fix prompt to forbid explanatory comments")
	}
}
