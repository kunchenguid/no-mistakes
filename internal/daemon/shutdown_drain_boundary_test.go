package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShutdown_DrainBoundaries(t *testing.T) {
	for _, mode := range []string{"deadline", "command-deadline", "approval", "next-step"} {
		t.Run(mode, func(t *testing.T) {
			p, database := newParkedRunEnv(t)
			repo, head := setupTestGitRepo(t, p, database, "drain-boundary")
			if mode == "command-deadline" {
				// Exercise the real configured-command outcome: cancellation kills
				// the shell, which reports a nonzero exit rather than a Go error.
				command := "echo ready > shutdown-command-started; while :; do sleep 1; done"
				if runtime.GOOS == "windows" {
					command = "echo ready > shutdown-command-started & ping -n 60 127.0.0.1 > NUL"
				}
				config := "auto_fix:\n  lint: 0\nallow_repo_commands: true\ncommands:\n  lint: '" + command + "'\n"
				if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(config), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
				gitCmd(t, repo.WorkingPath, "commit", "-m", "configure blocking command")
				gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
				head = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			}
			step := &drainStep{started: make(chan struct{}), release: make(chan struct{})}
			if mode == "approval" {
				step.outcome = &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"drain-question","severity":"warning","description":"operator decision","action":"ask-user"}]}`}
			}
			manager := NewRunManager(database, p, func() []pipeline.Step {
				if mode == "command-deadline" {
					return []pipeline.Step{&steps.LintStep{}}
				}
				if mode == "next-step" {
					return []pipeline.Step{step, &mockPassStep{name: types.StepLint}}
				}
				return []pipeline.Step{step}
			})
			runID, err := manager.startRun(context.Background(), repo, "main", head, head, "test", nil, "drain", "", false, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { manager.shutdown(0) })
			if mode == "command-deadline" {
				marker := filepath.Join(p.WorktreeDir(repo.ID, runID), "shutdown-command-started")
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(marker); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("configured command did not start")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				select {
				case <-step.started:
				case <-time.After(10 * time.Second):
					t.Fatal("step did not start")
				}
			}
			stopped := make(chan struct{})
			started := time.Now()
			grace := time.Second
			if mode == "deadline" || mode == "command-deadline" {
				grace = 50 * time.Millisecond
			}
			go func() { manager.shutdown(grace); close(stopped) }()
			deadline := time.Now().Add(5 * time.Second)
			for !manager.shuttingDown.Load() {
				if time.Now().After(deadline) {
					t.Fatal("shutdown did not begin")
				}
				time.Sleep(time.Millisecond)
			}
			if mode != "deadline" && mode != "command-deadline" {
				close(step.release)
			}
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown did not finish")
			}
			run, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			results, err := database.GetStepsByRun(runID)
			if err != nil || len(results) == 0 {
				t.Fatalf("steps = %+v, %v", results, err)
			}
			switch mode {
			case "deadline", "command-deadline":
				if time.Since(started) < grace {
					t.Fatal("cancelled the step before its drain grace elapsed")
				}
				if run.Status != types.RunFailed || run.Error == nil || *run.Error != "daemon shutting down" || run.AwaitingAgentSince != nil || results[0].Status != types.StepStatusFailed {
					t.Fatalf("deadline run = %+v, step = %+v", run, results[0])
				}
			case "approval":
				if run.Status != types.RunRunning || run.Error != nil || run.AwaitingAgentSince == nil || results[0].Status != types.StepStatusAwaitingApproval {
					t.Fatalf("parked run = %+v, step = %+v", run, results[0])
				}
				if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
					t.Fatalf("parked worktree lost: %v", err)
				}
				// Exercise real startup recovery: the drain did not invent a checkpoint
				// or approve anything; the existing approval gate accepts an operator.
				approveResumedRun(t, p, database, runID)
			case "next-step":
				if run.Status != types.RunFailed || run.Error == nil || *run.Error != "daemon shutting down" || len(results) != 2 || results[0].Status != types.StepStatusCompleted || results[1].Status != types.StepStatusPending {
					t.Fatalf("boundary run = %+v, steps = %+v", run, results)
				}
			}
		})
	}
}
