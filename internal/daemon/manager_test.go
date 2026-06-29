package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

func TestPushReceivedSkipStepsConfiguresExecutor(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	testStep := &mockPassStep{name: types.StepTest}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review, testStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "skip-run-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:      p.RepoDir("skip-run-repo"),
		Ref:       "refs/heads/main",
		Old:       "0000000000000000000000000000000000000000",
		New:       headSHA,
		SkipSteps: []types.StepName{types.StepReview},
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	if got := review.execCnt.Load(); got != 0 {
		t.Fatalf("review executed %d times, want 0", got)
	}
	if got := testStep.execCnt.Load(); got != 1 {
		t.Fatalf("test executed %d times, want 1", got)
	}
	steps, err := d.GetStepsByRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status != types.StepStatusSkipped {
			t.Fatalf("review status = %s, want %s", step.Status, types.StepStatusSkipped)
		}
	}
}

func TestPushReceivedAllowsDifferentBranchRunsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "concurrent-branch-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("concurrent-branch-repo"),
		Ref:  "refs/heads/feature/one",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &first); err != nil {
		t.Fatal(err)
	}
	waitForStartedBranch(t, started, "feature/one")

	var second ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("concurrent-branch-repo"),
		Ref:  "refs/heads/feature/two",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &second); err != nil {
		t.Fatal(err)
	}
	waitForStartedBranch(t, started, "feature/two")

	for _, tc := range []struct {
		branch string
		runID  string
	}{
		{branch: "feature/one", runID: first.RunID},
		{branch: "feature/two", runID: second.RunID},
	} {
		active, err := d.GetActiveRun("concurrent-branch-repo", tc.branch)
		if err != nil {
			t.Fatalf("get active run for %s: %v", tc.branch, err)
		}
		if active == nil {
			t.Fatalf("expected active run for %s", tc.branch)
		}
		if active.ID != tc.runID {
			t.Fatalf("active run for %s = %s, want %s", tc.branch, active.ID, tc.runID)
		}
		if active.Status != types.RunRunning {
			t.Fatalf("active run for %s status = %s, want running", tc.branch, active.Status)
		}
	}
}

type notifyBlockStep struct {
	name    types.StepName
	started chan<- string
}

func (s *notifyBlockStep) Name() types.StepName { return s.name }

func (s *notifyBlockStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.started <- sctx.Run.Branch:
	default:
	}
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func waitForStartedBranch(t *testing.T, started <-chan string, branch string) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case got := <-started:
			if got == branch {
				return
			}
		case <-timeout:
			t.Fatalf("run for branch %s did not start", branch)
		}
	}
}

// TestPushReceivedConcurrentDifferentBranchRunsAvoidSharedConfigLock fires two
// branch pushes for the same repo at the same time so both runs hit worktree
// creation and git-identity setup concurrently. All runs share one gate bare
// repo, so writing identity with `git config --local` (which targets the bare's
// shared config) made the two startups race on <bare>/config.lock and fail one
// run with "could not lock config file ...: File exists". CopyLocalUserIdentity
// now writes per-worktree, so the startups no longer contend. The race window
// is during synchronous startRun, so a failure surfaces directly as the
// push_received call's error. macOS-only in practice (Linux file locking and
// timing hide it), but the assertion is platform-independent.
func TestPushReceivedConcurrentDifferentBranchRunsAvoidSharedConfigLock(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	const repoID = "concurrent-config-lock-repo"
	_, headSHA := setupTestGitRepo(t, p, d, repoID)

	// Mirror a real gate: enable the per-worktree config isolation that
	// `no-mistakes init` installs, which is what lets identity writes avoid the
	// shared config.lock.
	if err := git.IsolateHooksPath(context.Background(), p.RepoDir(repoID)); err != nil {
		t.Fatalf("isolate hooks path: %v", err)
	}

	branches := []string{"feature/one", "feature/two"}
	errs := make([]error, len(branches))
	var wg sync.WaitGroup
	for i, br := range branches {
		wg.Add(1)
		go func(i int, br string) {
			defer wg.Done()
			// A dedicated client per goroutine: a single client serializes
			// calls, which would defeat the concurrency we are testing.
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				errs[i] = err
				return
			}
			defer client.Close()
			var res ipc.PushReceivedResult
			errs[i] = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repoID),
				Ref:  "refs/heads/" + br,
				Old:  "0000000000000000000000000000000000000000",
				New:  headSHA,
			}, &res)
		}(i, br)
	}
	wg.Wait()

	for i, br := range branches {
		if errs[i] != nil {
			t.Fatalf("concurrent push for %s failed: %v", br, errs[i])
		}
	}

	// Drain both start signals regardless of which run won the race to begin,
	// then confirm both branches have a live, error-free run.
	gotStarted := make(map[string]bool, len(branches))
	for range branches {
		select {
		case b := <-started:
			gotStarted[b] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("a concurrent run did not start (started so far: %v)", gotStarted)
		}
	}

	for _, br := range branches {
		if !gotStarted[br] {
			t.Fatalf("run for branch %s did not start", br)
		}
		active, err := d.GetActiveRun(repoID, br)
		if err != nil {
			t.Fatalf("get active run for %s: %v", br, err)
		}
		if active == nil {
			t.Fatalf("expected active run for %s", br)
		}
		if active.Status != types.RunRunning {
			t.Fatalf("active run for %s status = %s, want running (error: %v)", br, active.Status, active.Error)
		}
	}
}

func TestRerunSkipStepsConfiguresExecutor(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	testStep := &mockPassStep{name: types.StepTest}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review, testStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "skip-rerun-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("skip-rerun-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	var second ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:    "skip-rerun-repo",
		Branch:    "main",
		SkipSteps: []types.StepName{types.StepReview},
	}, &second)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, second.RunID)

	if got := review.execCnt.Load(); got != 1 {
		t.Fatalf("review executed %d times, want 1", got)
	}
	if got := testStep.execCnt.Load(); got != 2 {
		t.Fatalf("test executed %d times, want 2", got)
	}
	steps, err := d.GetStepsByRun(second.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status != types.StepStatusSkipped {
			t.Fatalf("review status = %s, want %s", step.Status, types.StepStatusSkipped)
		}
	}
}

func TestPushReceivedReturnsBeforeIntentSummarization(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	slowClaude := writeSlowMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+slowClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, headSHA := setupTestGitRepo(t, p, d, "intent-start-run-repo")
	writeManagerClaudeFixture(t, fakeHome, repo.WorkingPath, []string{
		`{"type":"user","cwd":` + testJSONString(t, repo.WorkingPath) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please update test.txt"}}`,
	})

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	started := time.Now()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("intent-start-run-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("PushReceived took %s, want under 2.5s", elapsed)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	waitForRunTerminalState(t, d, result.RunID)
}

func writeManagerClaudeFixture(t *testing.T, home, repoCWD string, lines []string) {
	t.Helper()
	encoded := testClaudeProjectDirName(repoCWD)
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-uuid-1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
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
