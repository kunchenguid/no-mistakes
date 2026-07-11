package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func TestSubscribeReceivesEvents(t *testing.T) {
	approvalStep := &mockApprovalStep{name: types.StepReview}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{approvalStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub1")

	// Trigger a push to get a run ID.
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub1"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for step to reach awaiting_approval.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steps, _ := d.GetStepsByRun(pushResult.RunID)
		for _, s := range steps {
			if s.Status == types.StepStatusAwaitingApproval {
				goto subscribeNow
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("step never reached awaiting_approval")

subscribeNow:
	// Subscribe to events for this run.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	// Approve the step to trigger completion events.
	var respondResult ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  pushResult.RunID,
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &respondResult)
	if err != nil {
		t.Fatal(err)
	}

	// Collect events until channel closes.
	var events []ipc.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				goto verifyEvents
			}
			events = append(events, event)
		case <-timeout:
			t.Fatal("subscriber channel never closed")
		}
	}

verifyEvents:
	if len(events) == 0 {
		t.Fatal("received no events")
	}
	hasRunCompleted := false
	for _, e := range events {
		if e.Type == ipc.EventRunCompleted {
			hasRunCompleted = true
		}
		if e.RunID != pushResult.RunID {
			t.Errorf("event run_id=%q, want %q", e.RunID, pushResult.RunID)
		}
	}
	if !hasRunCompleted {
		t.Error("never received run_completed event")
	}
}

func TestSubscribeToSlowRunReceivesEvents(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub2")

	// Trigger a push first.
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the slow step to start.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	// Subscribe to the running run.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	// Cancel the run (by sending another push, which cancels active runs).
	started2 := make(chan struct{})
	slowStep.started = started2

	var pushResult2 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult2)
	if err != nil {
		t.Fatal(err)
	}

	// The subscriber channel should close when the first run ends.
	eventCount := 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto done // channel closed
			}
			eventCount++
		case <-timeout:
			t.Fatal("subscriber channel never closed")
		}
	}
done:
	// We should have received at least the run events before channel closed.
	// The exact count depends on timing, but the channel MUST close.
}

func TestSubscribeToCompletedRunReturnsClosedChannel(t *testing.T) {
	// Use a fast step so the run completes quickly.
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepTest}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-sub-done")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-sub-done"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the run to complete by polling get_run.
	deadline := time.After(10 * time.Second)
	for {
		var result ipc.GetRunResult
		if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: pushResult.RunID}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Run != nil && (result.Run.Status == types.RunCompleted || result.Run.Status == types.RunFailed || result.Run.Status == types.RunCancelled) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("run did not complete in time")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Subscribe to the already-completed run. The channel should be immediately closed.
	ch, cancelSub, err := ipc.Subscribe(p.Socket(), &ipc.SubscribeParams{RunID: pushResult.RunID})
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed for completed run, but received an event")
		}
		// Channel closed - expected
	case <-time.After(5 * time.Second):
		t.Fatal("channel was not closed for completed run")
	}
}

func TestRecoverStaleRunsOnStartup(t *testing.T) {
	// Set up a DB with stale runs BEFORE starting the daemon.
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}

	// Create a "running" run with in-progress steps (simulating a crash).
	repo, err := d.InsertRepoWithID("stale-repo", "/tmp/stale-repo", "https://github.com/test/stale", "main")
	if err != nil {
		t.Fatal(err)
	}
	staleRun, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	d.UpdateRunStatus(staleRun.ID, types.RunRunning)
	staleStep, err := d.InsertStepResult(staleRun.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	d.StartStep(staleStep.ID)

	d.Close()

	// Start daemon — it should recover the stale run.
	d, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockPassStep{name: types.StepReview}}
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	// Verify the stale run was marked as failed.
	run, err := d.GetRun(staleRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed {
		t.Errorf("stale run status = %q, want %q", run.Status, types.RunFailed)
	}
	if run.Error == nil || *run.Error != "daemon crashed during execution" {
		t.Errorf("stale run error = %v, want %q", run.Error, "daemon crashed during execution")
	}

	// Verify the stale step was marked as failed.
	step, err := d.GetStepResult(staleStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != types.StepStatusFailed {
		t.Errorf("stale step status = %q, want %q", step.Status, types.StepStatusFailed)
	}
}

func TestRecoverOnStartup_ResumesParkedRun(t *testing.T) {
	tests := []struct {
		name             string
		demoMode         bool
		runnersAvailable bool
		wantStatus       types.RunStatus
	}{
		{
			name:             "non-demo with runnable candidates",
			runnersAvailable: true,
			wantStatus:       types.RunCompleted,
		},
		{
			name:       "demo with missing runner binaries",
			demoMode:   true,
			wantStatus: types.RunCompleted,
		},
		{
			name:       "non-demo with missing runner binaries",
			wantStatus: types.RunFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.demoMode {
				t.Setenv("NM_DEMO", "1")
			} else {
				t.Setenv("NM_DEMO", "0")
			}

			tmpDir, err := os.MkdirTemp("", "dtest")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
			p := paths.WithRoot(tmpDir)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			routing := config.DefaultRoutingConfig()
			executable := filepath.Join(tmpDir, "missing-agent-binary")
			if tc.runnersAvailable {
				executable, err = os.Executable()
				if err != nil {
					t.Fatal(err)
				}
			}
			for runner, spec := range routing.Runners {
				spec.Executable = executable + "-" + string(runner)
				if tc.runnersAvailable {
					spec.Executable = executable
				}
				routing.Runners[runner] = spec
			}
			globalConfig, err := yaml.Marshal(struct {
				Routing config.RoutingConfig `yaml:"routing"`
			}{Routing: routing})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), globalConfig, 0o644); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, headSHA := setupTestGitRepo(t, p, d, "resume-parked-run")
			run, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			worktree := p.WorktreeDir(repo.ID, run.ID)
			if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
			round, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.demoMode {
				if _, err := d.InsertStepResult(run.ID, types.StepTest); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.ParkApprovalGate(db.ParkApprovalGateInput{
				RunID: run.ID, StepResultID: step.ID, SourceRoundID: round.ID,
				Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 1,
			}); err != nil {
				t.Fatal(err)
			}
			parkedRun, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			var probe *recoveredDemoAgentProbeStep
			stepFactory := func() []pipeline.Step {
				execSteps := []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
				if tc.demoMode {
					probe = &recoveredDemoAgentProbeStep{observedAgent: make(chan string, 1)}
					execSteps = append(execSteps, probe)
				}
				return execSteps
			}
			mgr := NewRunManager(d, p, stepFactory)
			plan, prepareErr := mgr.prepareRecoveredRun(context.Background(), parkedRun)
			if tc.wantStatus == types.RunFailed {
				if prepareErr == nil || !strings.Contains(prepareErr.Error(), "no runnable candidate") {
					t.Fatalf("prepareRecoveredRun error = %v, want missing runnable candidate", prepareErr)
				}
			} else if prepareErr != nil {
				t.Fatalf("prepareRecoveredRun: %v", prepareErr)
			}
			if tc.demoMode {
				if plan.agent == nil || plan.agent.Name() != "noop" {
					t.Fatalf("recovered demo agent = %v, want noop", plan.agent)
				}
			}

			errCh := make(chan error, 1)
			go func() {
				errCh <- RunWithOptions(p, d, stepFactory)
			}()
			startupDeadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(p.Socket()); err == nil {
					break
				}
				select {
				case err := <-errCh:
					t.Fatalf("daemon stopped before binding IPC socket: %v", err)
				default:
				}
				if time.Now().After(startupDeadline) {
					t.Fatal("daemon did not bind IPC socket")
				}
				time.Sleep(20 * time.Millisecond)
			}
			defer func() {
				client, err := ipc.Dial(p.Socket())
				if err == nil {
					_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
					_ = client.Close()
				}
				select {
				case <-errCh:
				case <-time.After(3 * time.Second):
					t.Error("daemon did not stop")
				}
			}()

			if tc.wantStatus == types.RunCompleted {
				deadline := time.Now().Add(5 * time.Second)
				var lastErr error
				for {
					if time.Now().After(deadline) {
						recovered, getErr := d.GetRun(run.ID)
						t.Fatalf("recovered gate never accepted an approval: last error %v, run %#v, get run error %v", lastErr, recovered, getErr)
					}
					client, err := ipc.Dial(p.Socket())
					if err == nil {
						var response ipc.RespondResult
						err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
							RunID:  run.ID,
							Step:   types.StepReview,
							Action: types.ActionApprove,
						}, &response)
						_ = client.Close()
						if err == nil {
							break
						}
						lastErr = err
					} else {
						lastErr = err
					}
					time.Sleep(20 * time.Millisecond)
				}
			}

			completed := waitForRunTerminalState(t, d, run.ID)
			if completed.Status != tc.wantStatus {
				t.Fatalf("recovered run status = %s, want %s (error: %v)", completed.Status, tc.wantStatus, completed.Error)
			}
			if completed.AwaitingAgentSince != nil {
				t.Fatal("recovered run remained parked")
			}
			if tc.demoMode {
				select {
				case got := <-probe.observedAgent:
					if got != "noop" {
						t.Fatalf("recovered demo agent = %q, want noop", got)
					}
				default:
					t.Fatal("recovered demo run did not route an invocation through the no-op agent")
				}
			}

			// The executor marks the run terminal before its owner goroutine performs
			// worktree cleanup. Wait for that cleanup rather than assuming it completed
			// in the same scheduling slice, which is especially unreliable on Windows.
			cleanupDeadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(worktree); os.IsNotExist(err) {
					break
				} else if err != nil {
					t.Fatalf("stat recovered worktree: %v", err)
				}
				if time.Now().After(cleanupDeadline) {
					t.Fatalf("recovered worktree still exists after cleanup: %s", worktree)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

type recoveredDemoAgentProbeStep struct {
	observedAgent chan string
}

func (s *recoveredDemoAgentProbeStep) Name() types.StepName { return types.StepTest }

func (s *recoveredDemoAgentProbeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Agent == nil {
		return nil, fmt.Errorf("recovered demo step has no injected agent")
	}
	if sctx.Agent.Name() != "noop" {
		return nil, fmt.Errorf("recovered demo step agent = %q, want noop", sctx.Agent.Name())
	}
	if sctx.Invoker == nil {
		return nil, fmt.Errorf("recovered demo step has no routing invoker")
	}
	if _, err := sctx.Invoker.Invoke(sctx.Ctx, agent.InvocationRequest{
		Purpose: types.PurposeIntentSummarization,
		Scope:   sctx.InvocationScope,
		Payload: agent.RunOpts{Prompt: "exercise recovered demo routing"},
	}); err != nil {
		return nil, fmt.Errorf("invoke recovered demo agent: %w", err)
	}
	s.observedAgent <- sctx.Agent.Name()
	return &pipeline.StepOutcome{}, nil
}

func TestRecoverCleansUpOrphanedWorktrees(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Create orphaned worktree directories.
	orphanDir := p.WorktreeDir("some-repo", "some-run")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(orphanDir, "test.txt"), []byte("orphan"), 0o644)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockPassStep{name: types.StepReview}}
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})

	// Orphaned worktree directory should be removed.
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphaned worktree dir still exists: %s", orphanDir)
	}
}

// TestRecoverIsolatesGateRepoHooksPath covers issue #122 for existing
// installs: bare repos created before the fix have no per-worktree
// core.hookspath, so a husky pollution still disables their hook.
// Daemon startup must migrate them in place.
func TestRecoverIsolatesGateRepoHooksPath(t *testing.T) {
	tmpDir := t.TempDir()
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Simulate an existing install: a bare repo created the old way
	// (without IsolateHooksPath) whose shared local config has been
	// poisoned by husky during a prior pipeline run.
	bareDir := p.RepoDir("legacy-repo")
	ctx := context.Background()
	if err := gitpkg.InitBare(ctx, bareDir); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", bareDir, "config", "core.hookspath", ".husky/_").CombinedOutput(); err != nil {
		t.Fatalf("seed poisoned config: %v: %s", err, out)
	}

	migrateGateConfigs(ctx, p)

	// Effective core.hookspath should now resolve to the bare's hooks dir.
	out, err := exec.Command("git", "-C", bareDir, "config", "--get", "core.hookspath").Output()
	if err != nil {
		t.Fatalf("get core.hookspath: %v", err)
	}
	want, err := filepath.Abs(filepath.Join(bareDir, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("after migration, core.hookspath = %q, want %q", got, want)
	}
	out, err = exec.Command("git", "-C", bareDir, "config", "--get", "receive.advertisePushOptions").Output()
	if err != nil {
		t.Fatalf("get receive.advertisePushOptions: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Fatalf("receive.advertisePushOptions = %q, want true", got)
	}
}

func TestRecoverRefreshesLegacyManagedGateHook(t *testing.T) {
	tmpDir := t.TempDir()
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	bareDir := p.RepoDir("legacy-repo")
	ctx := context.Background()
	if err := gitpkg.InitBare(ctx, bareDir); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	legacyHook := `#!/bin/sh
# no-mistakes post-receive hook
# Notify daemon of push. Non-blocking - push always succeeds.
NM_BIN='/usr/local/bin/no-mistakes'
while read oldrev newrev refname; do
  NM_HOOK_HELPER=1 "$NM_BIN" daemon notify-push \
    --gate "$(pwd)" \
    --ref "$refname" \
    --old "$oldrev" \
    --new "$newrev" >/dev/null 2>&1 || true
done
exit 0
`
	if err := os.WriteFile(hookPath, []byte(legacyHook), 0o755); err != nil {
		t.Fatal(err)
	}

	migrateGateConfigs(ctx, p)

	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "GIT_PUSH_OPTION_COUNT") {
		t.Fatalf("migrated hook should forward git push options, got:\n%s", content)
	}
	if !strings.Contains(content, "--push-option") {
		t.Fatalf("migrated hook should pass push options to notify-push, got:\n%s", content)
	}
	if strings.Contains(content, ">/dev/null 2>&1 || true") {
		t.Fatalf("migrated hook should not silently swallow notify-push errors, got:\n%s", content)
	}
}

func TestRecoverStaleUtilityInvocationsLeavesLiveOwnerActive(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	attemptID := startWizardUtilityAttempt(t, database, os.Getpid())

	recoverStaleUtilityInvocations(database)

	attempt, err := database.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if attempt == nil || attempt.Terminal != nil {
		t.Fatalf("live-owner attempt = %+v, want active", attempt)
	}
}

func TestRecoverStaleUtilityInvocationsInterruptsDeadOwner(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	attemptID := startWizardUtilityAttempt(t, database, 999999)

	recoverStaleUtilityInvocations(database)

	attempt, err := database.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if attempt == nil || attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeInterrupted {
		t.Fatalf("dead-owner attempt = %+v, want interrupted", attempt)
	}
}

func startWizardUtilityAttempt(t *testing.T, database *db.DB, ownerPID int) string {
	t.Helper()
	scope, err := database.InsertUtilityScope(types.UtilityScopeWizard, ownerPID)
	if err != nil {
		t.Fatalf("insert utility scope: %v", err)
	}
	attemptID, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeBranchCommitSuggestion,
		Role:         types.InvocationRoleFixer,
		Scope:        types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: scope.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start utility invocation: %v", err)
	}
	return attemptID
}
