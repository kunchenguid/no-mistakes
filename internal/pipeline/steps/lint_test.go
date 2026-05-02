package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

func TestLintStep_ConfiguredCommand_UsesStepEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binDir := fakeCLIBinDir(t)
	logFile := filepath.Join(t.TempDir(), "lint-command.log")
	linkTestBinary(t, binDir, "nm-lintcmd")

	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{Lint: "nm-lintcmd"})
	sctx.Env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE": "record-success",
		"FAKE_CLI_LOG":  logFile,
	})

	step := &LintStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected configured lint command from StepContext env to pass")
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "nm-lintcmd") {
		t.Fatalf("expected env-resolved lint command to run, got %q", string(logData))
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

func TestLintStep_PassingCommand(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestLintStep_PromptIncludesAction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	findings := Findings{Items: nil, Summary: "all clean"}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	step := &LintStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "action") {
		t.Error("expected lint prompt to instruct agent about action")
	}
}
