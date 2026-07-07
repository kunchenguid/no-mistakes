package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestLintStep_FixMode_CommitsChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"lint-1 =======","severity":"warning","file":"internal/pipeline/steps/lint.go >>>>>>> prompt","description":"linter found issues (exit code 1) <<<<<<< HEAD"}],"summary":"main.go:10: unused variable x ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  'fix lint issues,'  "}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

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
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "unused variable x") {
		t.Error("expected fix prompt to contain previous lint summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "lint-1 =======") {
		t.Error("expected lint fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "lint.go >>>>>>> prompt") {
		t.Error("expected lint fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected lint fix prompt to exclude merge markers")
	}
	if !strings.Contains(ag.calls[0].Prompt, "smallest correct root-cause fix") {
		t.Error("expected lint fix prompt to prefer root-cause fixes over bandaids")
	}
	if strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected lint fix prompt not to prefer narrow minimal changes")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fix lint issues" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`not json`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	sctx.Fixing = true

	step := &LintStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fix lint issues" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_NoConfiguredLint_CommitsAgentFixesWithoutApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"format code"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when agent fixed no-config lint issues")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix loop when no unresolved lint issues remain")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call, got %d", callCount)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after lint fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): format code" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestLintStep_NoConfiguredLint_UnresolvedFindingsNeedApprovalWithoutAutoFixLoop(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"prettier still fails","action":"auto-fix"}],"summary":"lint still fails"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval for unresolved no-config lint findings")
	}
	if outcome.AutoFixable {
		t.Error("expected unresolved no-config lint findings not to auto-fix again")
	}
	if !strings.Contains(ag.calls[0].Prompt, "only unresolved") {
		t.Error("expected no-config lint prompt to report only unresolved issues")
	}
}
