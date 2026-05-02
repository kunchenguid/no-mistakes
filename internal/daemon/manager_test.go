package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// --- RunManager unit tests ---

func TestRepoIDFromGatePath(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"/home/user/.no-mistakes/repos/abc123.git", "abc123", false},
		{"/tmp/repos/test-id.git", "test-id", false},
		{"/tmp/repos/nope", "", true},
	}
	for _, tc := range tests {
		got, err := repoIDFromGatePath(tc.path)
		if (err != nil) != tc.wantErr {
			t.Errorf("repoIDFromGatePath(%q): err=%v, wantErr=%v", tc.path, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("repoIDFromGatePath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// --- RunManager integration tests ---

func TestPushReceivedTracksRunTelemetry(t *testing.T) {
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "telemetry-run-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("telemetry-run-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}

	started := recorder.find("run", "action", "started")
	if started == nil {
		t.Fatal("expected run started telemetry event")
	}
	if got := started.fields["trigger"]; got != "push" {
		t.Fatalf("started trigger = %v, want push", got)
	}
	if got := started.fields["agent"]; got != string(types.AgentClaude) {
		t.Fatalf("started agent = %v, want %q", got, types.AgentClaude)
	}
	if got := started.fields["branch_role"]; got != "default" {
		t.Fatalf("started branch_role = %v, want default", got)
	}

	finished := recorder.find("run", "action", "finished")
	if finished == nil {
		t.Fatal("expected run finished telemetry event")
	}
	if got := finished.fields["status"]; got != string(types.RunCompleted) {
		t.Fatalf("finished status = %v, want %q", got, types.RunCompleted)
	}
	if _, ok := finished.fields["duration_ms"]; !ok {
		t.Fatal("expected duration_ms in run finished telemetry")
	}
}

func TestPushReceivedTracksRunTelemetryAfterPanic(t *testing.T) {
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	step := &mockPanicStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "telemetry-panic-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("telemetry-panic-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Error != nil && strings.Contains(*run.Error, "internal panic") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finished := recorder.find("run", "action", "finished")
	if finished == nil {
		t.Fatal("expected run finished telemetry event after panic")
	}
	if got := finished.fields["status"]; got != string(types.RunFailed) {
		t.Fatalf("finished status = %v, want %q", got, types.RunFailed)
	}
	if _, ok := finished.fields["duration_ms"]; !ok {
		t.Fatal("expected duration_ms in run finished telemetry after panic")
	}
}

func TestPushReceivedDemoModeBypassesAgentResolution(t *testing.T) {
	t.Setenv("NM_DEMO", "1")

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: /path/that/does/not/exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-demo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-demo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	waitForRunTerminalState(t, d, result.RunID)
	run, err := d.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want %q (error: %s)", run.Status, types.RunCompleted, runErr)
	}
	if step.execCnt.Load() == 0 {
		t.Error("mock step was never executed")
	}
}

func TestPushReceivedCancelsActiveRun(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo2")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// First push — starts a slow pipeline.
	var result1 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result1)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the slow step to start executing.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	// Second push — should cancel first run.
	// Need a new started channel for the second run's step.
	started2 := make(chan struct{})
	slowStep.started = started2

	var result2 ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo2"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result2)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for first run to be marked as failed/cancelled.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run1, err := d.GetRun(result1.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run1.Status == types.RunCancelled {
			if run1.Error == nil || !strings.Contains(*run1.Error, "superseded by new push") {
				var got string
				if run1.Error != nil {
					got = *run1.Error
				}
				t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
			}
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("first run was not cancelled within timeout")
}

func TestCancelRunStopsActivePipeline(t *testing.T) {
	started := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: started}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-cancel")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-cancel"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow step never started")
	}

	var cancelResult ipc.CancelRunResult
	err = client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: pushResult.RunID}, &cancelResult)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelResult.OK {
		t.Fatal("cancel run should return OK")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCancelled {
			if run.Error == nil || !strings.Contains(*run.Error, "aborted by user") {
				var got string
				if run.Error != nil {
					got = *run.Error
				}
				t.Fatalf("expected cancelled run error to mention aborted by user, got %q", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run was not cancelled within timeout")
}

func TestPushReceivedDoesNotCancelActiveRunOnDifferentBranch(t *testing.T) {
	startedMain := make(chan struct{})
	slowStep := &mockSlowStep{name: types.StepReview, started: startedMain}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slowStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-different-branch")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var mainPush ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-different-branch"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &mainPush)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-startedMain:
	case <-time.After(5 * time.Second):
		t.Fatal("main branch run never started")
	}

	startedFeature := make(chan struct{})
	slowStep.started = startedFeature

	var featurePush ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo-different-branch"),
		Ref:  "refs/heads/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &featurePush)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-startedFeature:
	case <-time.After(5 * time.Second):
		t.Fatal("feature branch run never started")
	}

	time.Sleep(200 * time.Millisecond)

	mainRun, err := d.GetRun(mainPush.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if mainRun.Status == types.RunFailed || mainRun.Status == types.RunCancelled {
		if mainRun.Error != nil && strings.Contains(*mainRun.Error, "superseded by new push") {
			t.Fatalf("main branch run should not be superseded by a push to a different branch: %q", *mainRun.Error)
		}
		t.Fatalf("main branch run should still be active, got status %s", mainRun.Status)
	}
}

func TestRespondToActiveRun(t *testing.T) {
	approvalStep := &mockApprovalStep{name: types.StepReview}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{approvalStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo3")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Start a pipeline that will pause for approval.
	var pushResult ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("testrepo3"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushResult)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for step to reach awaiting_approval status.
	deadline := time.Now().Add(5 * time.Second)
	awaitingApproval := false
	for time.Now().Before(deadline) {
		steps, err := d.GetStepsByRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.Status == types.StepStatusAwaitingApproval {
				awaitingApproval = true
				break
			}
		}
		if awaitingApproval {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !awaitingApproval {
		t.Fatal("step never reached awaiting_approval status")
	}

	// Send approve action.
	var respondResult ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  pushResult.RunID,
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &respondResult)
	if err != nil {
		t.Fatal(err)
	}
	if !respondResult.OK {
		t.Error("respond should return OK")
	}

	// Wait for run to complete.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(pushResult.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCompleted {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("run did not complete after approval")
}

func TestPushReceivedCleansUpWorktreeOnConfigFailure(t *testing.T) {
	// Set up a standalone RunManager (no daemon) to test worktree cleanup
	// when config loading fails after worktree creation.
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
	t.Cleanup(func() { d.Close() })

	// Set up a real git repo and bare repo.
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	repoID := "wt-cleanup-repo"
	bareDir := p.RepoDir(repoID)
	gitCmd(t, "", "init", "--bare", bareDir)
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")

	_, err = d.InsertRepoWithID(repoID, workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	// Write an invalid config.yaml to cause LoadGlobal to fail.
	if err := os.WriteFile(p.ConfigFile(), []byte("invalid: yaml: [[["), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	// HandlePushReceived should fail due to invalid config, but clean up the worktree.
	_, err = mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	})
	if err == nil {
		t.Fatal("expected error from invalid config")
	}

	// Verify worktree directory was cleaned up.
	wtRoot := filepath.Join(p.WorktreesDir(), repoID)
	entries, err := os.ReadDir(wtRoot)
	if err == nil && len(entries) > 0 {
		t.Errorf("worktree directory not cleaned up, found %d entries in %s", len(entries), wtRoot)
	}
}

func TestRespondNoActiveExecutor(t *testing.T) {
	p, _ := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID:  "nonexistent",
		Step:   types.StepReview,
		Action: types.ActionApprove,
	}, &result)
	if err == nil {
		t.Error("expected error when no active executor for run")
	}
}
