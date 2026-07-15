package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type reconcileEnvStep struct {
	step pipeline.Step
	env  []string
}

func (s reconcileEnvStep) Name() types.StepName { return s.step.Name() }
func (s reconcileEnvStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env = s.env
	return s.step.Execute(ctx)
}
func (s reconcileEnvStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	ctx.Env = s.env
	return s.step.(pipeline.ApprovalGateReconciler).ReconcileApprovalGate(ctx)
}

func TestCIGateReconciliationClearsActiveRunAfterPRBecomesTerminal(t *testing.T) {
	for _, terminalState := range []string{"MERGED", "CLOSED"} {
		t.Run(terminalState, func(t *testing.T) {
			database, p, run, repo, dir, statePath, env := setupCIGateReconcileTest(t)
			exec := pipeline.NewExecutor(database, p, &config.Config{Agent: types.AgentClaude, CITimeout: 80 * time.Millisecond}, &mockAgent{name: "test"}, []pipeline.Step{
				reconcileEnvStep{step: &PRStep{}, env: env},
				reconcileEnvStep{step: &CIStep{pollIntervalOverride: 10 * time.Millisecond}, env: env},
			}, nil)
			exec.SetGateReconcileTimings(20*time.Millisecond, 5*time.Second)

			done := make(chan error, 1)
			go func() { done <- exec.Execute(context.Background(), run, repo, dir) }()
			waitForCIGate(t, database, run.ID)
			if err := os.WriteFile(statePath, []byte(terminalState+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s PR did not reconcile", terminalState)
			}

			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			active, err := lifecycle.ActiveRuns(p)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunCompleted || persisted.AwaitingAgentSince != nil || len(active) != 0 {
				t.Fatalf("terminal PR reconciliation: status=%s awaiting=%v active=%d", persisted.Status, persisted.AwaitingAgentSince, len(active))
			}
		})
	}
}

func TestCIGateReconciliationPreservesOpenErrorAndUnknownStates(t *testing.T) {
	for _, state := range []string{"OPEN", "ERROR", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			database, p, run, repo, dir, statePath, env := setupCIGateReconcileTest(t)
			exec := pipeline.NewExecutor(database, p, &config.Config{Agent: types.AgentClaude, CITimeout: 80 * time.Millisecond}, &mockAgent{name: "test"}, []pipeline.Step{
				reconcileEnvStep{step: &PRStep{}, env: env},
				reconcileEnvStep{step: &CIStep{pollIntervalOverride: 10 * time.Millisecond}, env: env},
			}, nil)
			exec.SetGateReconcileTimings(20*time.Millisecond, 5*time.Second)

			done := make(chan error, 1)
			go func() { done <- exec.Execute(context.Background(), run, repo, dir) }()
			waitForCIGate(t, database, run.ID)
			if err := os.WriteFile(statePath, []byte(state+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(60 * time.Millisecond)

			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			active, err := lifecycle.ActiveRuns(p)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunRunning || persisted.AwaitingAgentSince == nil || len(active) != 1 {
				t.Fatalf("state %s did not fail closed: status=%s awaiting=%v active=%d", state, persisted.Status, persisted.AwaitingAgentSince, len(active))
			}
			if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("approval did not finish preserved gate")
			}
		})
	}
}

func setupCIGateReconcileTest(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo, string, string, []string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}

	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	statePath := filepath.Join(t.TempDir(), "pr-state")
	if err := os.WriteFile(statePath, []byte("OPEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":       "ci-gh-reconcile",
		"FAKE_CLI_STATE_PATH": statePath,
	})
	return database, p, run, repo, dir, statePath, env
}

func waitForCIGate(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil && len(steps) == 2 && steps[1].Status == types.StepStatusAwaitingApproval {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CI step did not reach awaiting_approval")
}
