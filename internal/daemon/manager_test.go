package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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

	// The executor persists terminal status before its owner goroutine emits
	// terminal telemetry. Wait for that asynchronous handoff instead of
	// assuming it completed in the same scheduling slice, which is especially
	// unreliable on Windows.
	finished := waitForTelemetryEvent(t, recorder, "run", "action", "finished")
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

type capturedForgeContext struct {
	repoID   string
	provider scm.Provider
	env      map[string]string
	gitEnv   string
}

type captureForgeContextStep struct {
	contexts chan<- capturedForgeContext
}

func (s *captureForgeContextStep) Name() types.StepName { return types.StepReview }
func (s *captureForgeContextStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.ForgeContext == nil {
		return nil, fmt.Errorf("forge context is missing")
	}
	gitEnv, err := captureGitForgeEnvironment(sctx)
	if err != nil {
		return nil, err
	}
	s.contexts <- capturedForgeContext{
		repoID:   sctx.Repo.ID,
		provider: sctx.ForgeContext.Provider,
		env:      testEnvMap(sctx.ForgeContext.Environment.Apply([]string{"GH_TOKEN=ambient"})),
		gitEnv:   gitEnv,
	}
	return &pipeline.StepOutcome{}, nil
}

type barrierForgeContextStep struct {
	contexts chan<- capturedForgeContext
	release  <-chan struct{}
}

func (s *barrierForgeContextStep) Name() types.StepName { return types.StepReview }
func (s *barrierForgeContextStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.ForgeContext == nil {
		return nil, fmt.Errorf("forge context is missing")
	}
	gitEnv, err := captureGitForgeEnvironment(sctx)
	if err != nil {
		return nil, err
	}
	s.contexts <- capturedForgeContext{
		repoID:   sctx.Repo.ID,
		provider: sctx.ForgeContext.Provider,
		env:      testEnvMap(sctx.ForgeContext.Environment.Apply([]string{"GH_TOKEN=ambient"})),
		gitEnv:   gitEnv,
	}
	select {
	case <-s.release:
		return &pipeline.StepOutcome{}, nil
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
}

func captureGitForgeEnvironment(sctx *pipeline.StepContext) (string, error) {
	return git.Run(
		sctx.Ctx,
		sctx.WorkDir,
		"-c",
		"alias.show-forge=!printf 'config:%s token:%s' \"$GH_CONFIG_DIR\" \"${GH_TOKEN:+set}\"",
		"show-forge",
	)
}

func TestPushReceivedResolvesForgeProfileIntoRunContext(t *testing.T) {
	const credentialSentinel = "forge-secret-must-not-persist"
	t.Setenv("GH_TOKEN", credentialSentinel)
	contexts := make(chan capturedForgeContext, 1)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&captureForgeContextStep{contexts: contexts}}
	})

	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n    users:\n        test-user:\n    user: test-user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	globalConfig, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	globalConfig = append(globalConfig, []byte(fmt.Sprintf("forge_profiles:\n  github.com:\n    gh_config_dir: %s\n", profileDir))...)
	if err := os.WriteFile(p.ConfigFile(), globalConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	repo, headSHA := setupTestGitRepo(t, p, d, "forge-profile-run-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}

	select {
	case resolved := <-contexts:
		if resolved.provider != scm.ProviderGitHub {
			t.Fatalf("provider = %q, want %q", resolved.provider, scm.ProviderGitHub)
		}
		if resolved.env["GH_CONFIG_DIR"] != profileDir {
			t.Fatalf("GH_CONFIG_DIR = %q, want %q", resolved.env["GH_CONFIG_DIR"], profileDir)
		}
		if _, exists := resolved.env["GH_TOKEN"]; exists {
			t.Fatal("ambient GH_TOKEN survived run context")
		}
		if resolved.gitEnv != "config:"+profileDir+" token:" {
			t.Fatalf("git subprocess environment = %q", resolved.gitEnv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not report its forge context")
	}
	if run := waitForRunTerminalState(t, d, result.RunID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	if err := filepath.WalkDir(p.Root(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), credentialSentinel) {
			return fmt.Errorf("credential sentinel persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPushReceivedKeepsConcurrentForgeProfilesIsolated(t *testing.T) {
	contexts := make(chan capturedForgeContext, 2)
	release := make(chan struct{})
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&barrierForgeContextStep{contexts: contexts, release: release}}
	})

	personalDir := t.TempDir()
	workDir := t.TempDir()
	for dir, host := range map[string]string{personalDir: "personal.example.test", workDir: "work.example.test"} {
		if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(host+":\n    user: test-user\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	globalConfig, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	globalConfig = append(globalConfig, []byte(fmt.Sprintf(
		"forge_profiles:\n  personal.example.test:\n    gh_config_dir: %s\n  work.example.test:\n    gh_config_dir: %s\n",
		personalDir, workDir,
	))...)
	if err := os.WriteFile(p.ConfigFile(), globalConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	type runRef struct {
		id     string
		result ipc.PushReceivedResult
	}
	runs := make([]runRef, 0, 2)
	for _, tc := range []struct {
		id   string
		host string
	}{
		{id: "personal-forge-run", host: "personal.example.test"},
		{id: "work-forge-run", host: "work.example.test"},
	} {
		repo, headSHA := setupTestGitRepo(t, p, d, tc.id)
		if _, err := d.UpdateRepoMetadata(repo.ID, "https://"+tc.host+"/test/repo.git", "main"); err != nil {
			t.Fatal(err)
		}
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			t.Fatal(err)
		}
		var result ipc.PushReceivedResult
		err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: headSHA,
		}, &result)
		_ = client.Close()
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, runRef{id: repo.ID, result: result})
	}

	observed := make(map[string]capturedForgeContext, 2)
	for range 2 {
		select {
		case captured := <-contexts:
			observed[captured.repoID] = captured
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent forge runs did not reach barrier")
		}
	}
	for repoID, wantDir := range map[string]string{"personal-forge-run": personalDir, "work-forge-run": workDir} {
		captured := observed[repoID]
		if got := captured.env["GH_CONFIG_DIR"]; got != wantDir {
			t.Fatalf("%s GH_CONFIG_DIR = %q, want %q", repoID, got, wantDir)
		}
		if _, exists := captured.env["GH_TOKEN"]; exists {
			t.Fatalf("%s retained ambient GH_TOKEN", repoID)
		}
		if captured.gitEnv != "config:"+wantDir+" token:" {
			t.Fatalf("%s git subprocess environment = %q", repoID, captured.gitEnv)
		}
	}
	close(release)
	for _, run := range runs {
		if completed := waitForRunTerminalState(t, d, run.result.RunID); completed.Status != types.RunCompleted {
			t.Fatalf("%s status = %s", run.id, completed.Status)
		}
	}
}

func testEnvMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
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
	// The 3s slowClaude script is not on this test's synchronous path (the
	// review step here is a mockPassStep and the "claude" agent is explicit,
	// so ResolveAgent never probes it): what this bound really guards is
	// startRun's synchronous git plumbing (worktree add, identity copy,
	// fetch, resolve-ref, config loads) staying well clear of the 3s the
	// pipeline goroutine's slow agent call would take if it ever ran inline.
	// Windows CI process-spawn overhead across those several git subprocess
	// calls is much higher than on macOS/Linux, so Windows gets generous
	// headroom while non-Windows keeps the tight bound that would catch a
	// real regression in startRun's synchronous git plumbing.
	maxElapsed := 2500 * time.Millisecond
	if runtimeGOOS == "windows" {
		maxElapsed = 8 * time.Second
	}
	if elapsed := time.Since(started); elapsed > maxElapsed {
		t.Fatalf("PushReceived took %s, want under %s", elapsed, maxElapsed)
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
	for _, field := range []string{"agent_invocations", "resumed_invocations", "fallback_invocations"} {
		if got, ok := finished.fields[field]; !ok || got != 0 {
			t.Fatalf("%s = %v, want 0", field, got)
		}
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
