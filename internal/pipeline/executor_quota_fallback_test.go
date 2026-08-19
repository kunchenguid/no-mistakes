package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type quotaFixAgent struct {
	name    string
	workDir string
	calls   int
}

func (a *quotaFixAgent) Name() string { return a.name }
func (a *quotaFixAgent) Close() error { return nil }

func (a *quotaFixAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if a.name == "claude" && a.calls == 2 {
		if err := os.WriteFile(filepath.Join(a.workDir, "prior-fix.txt"), []byte("preserve this fix\n"), 0o644); err != nil {
			return nil, err
		}
		quotaTestGit(a.workDir, "add", "prior-fix.txt")
		quotaTestGit(a.workDir, "commit", "-m", "prior pipeline fix")
		err := errors.New("claude exited: exit status 1: session limit reached")
		return nil, agent.ClassifyProviderError(err, "session limit reached")
	}
	return &agent.Result{Text: "validated"}, nil
}

func quotaTestGit(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		panic("git " + strings.Join(args, " ") + ": " + string(output) + ": " + err.Error())
	}
	return strings.TrimSpace(string(output))
}

func initQuotaTestRepo(t *testing.T, dir string) {
	t.Helper()
	quotaTestGit(dir, "init", "-b", "main")
	quotaTestGit(dir, "config", "user.name", "test")
	quotaTestGit(dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	quotaTestGit(dir, "add", "base.txt")
	quotaTestGit(dir, "commit", "-m", "base")
}

func TestExecutor_QuotaFallbackPreservesActiveFixRoundAndPriorCommit(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initQuotaTestRepo(t, workDir)

	claude := &quotaFixAgent{name: "claude", workDir: workDir}
	pi := &quotaFixAgent{name: "pi", workDir: workDir}
	fallback := agent.NewFallback([]agent.Agent{claude, pi})
	findings := `{"findings":[{"id":"keep-me","severity":"error","description":"fix it","action":"ask-user"}],"summary":"one finding"}`
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Prompt: "review"}); err != nil {
				return nil, err
			}
			if !sctx.Fixing {
				return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
			}
			return &StepOutcome{Findings: `{"findings":[],"summary":"fixed"}`}, nil
		},
	}
	cfg := &config.Config{Agent: types.AgentClaude, SessionReuse: false}
	exec := NewExecutor(database, p, cfg, fallback, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"keep-me"}); err != nil {
		t.Fatalf("respond with fix: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("execute: %v", err)
	}

	gotRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.ID != run.ID || gotRun.Status != types.RunCompleted {
		t.Fatalf("run = %+v, want same completed run", gotRun)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].StepName != types.StepReview || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("steps = %+v, want one completed review step", steps)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 2 || rounds[0].FindingsJSON == nil || !strings.Contains(*rounds[0].FindingsJSON, "keep-me") {
		t.Fatalf("rounds = %+v, want original finding and one fix round", rounds)
	}
	if claude.calls != 2 || pi.calls != 1 {
		t.Fatalf("agent calls = claude %d pi %d, want quota during fix then one fallback", claude.calls, pi.calls)
	}
	if message := quotaTestGit(workDir, "log", "-1", "--pretty=%s"); message != "prior pipeline fix" {
		t.Fatalf("prior fix commit = %q, want it preserved", message)
	}
}

type quotaOnceAgent struct {
	name  string
	err   error
	calls int
}

func (a *quotaOnceAgent) Name() string { return a.name }
func (a *quotaOnceAgent) Close() error { return nil }

func (a *quotaOnceAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if a.calls == 1 {
		return nil, a.err
	}
	return &agent.Result{Text: "validated"}, nil
}

func TestExecutor_QuotaFallbackAppliesToOtherAgentDrivenSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	first := &quotaOnceAgent{name: "claude", err: agent.ClassifyProviderError(errors.New("claude exited: exit status 1: rate_limit_error"), "rate_limit_error")}
	second := &quotaFixAgent{name: "pi"}
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "collect targeted evidence"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, agent.NewFallback([]agent.Agent{first, second}), []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("agent calls = claude %d pi %d, want 1/1", first.calls, second.calls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].StepName != types.StepTest || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("steps = %+v, want one completed test step", steps)
	}
}
