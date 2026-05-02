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

func TestLintStep_NoCommand_MalformedAgentOutput(t *testing.T) {
	t.Parallel()
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

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
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
