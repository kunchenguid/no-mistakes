package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_HandoffWakeRechecksGreenReadiness(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
	sctx.Config.CITimeout = time.Minute
	wake := make(chan struct{}, 1)
	sctx.CIWake = wake
	firstReady := make(chan struct{})
	readyCalls := 0
	sctx.SetCIReadiness = func(ready, _ bool) error {
		if !ready {
			return nil
		}
		readyCalls++
		if readyCalls == 1 {
			close(firstReady)
			return nil
		}
		return pipeline.ErrHandoff
	}

	done := make(chan struct {
		err error
	}, 1)
	go func() {
		_, err := (&CIStep{pollIntervalOverride: 30 * time.Second}).Execute(sctx)
		done <- struct{ err error }{err: err}
	}()
	<-firstReady
	wake <- struct{}{}

	select {
	case result := <-done:
		if !errors.Is(result.err, pipeline.ErrHandoff) {
			t.Fatalf("handoff result error = %v, want ErrHandoff", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handoff wake did not interrupt the CI poll wait")
	}
}

func TestCIStep_ReadinessWriteFailureWarnsAndKeepsMonitoring(t *testing.T) {
	for _, tc := range []struct {
		name, checks, warning string
	}{
		{"persist", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "could not persist CI readiness"},
		{"clear", `invalid check response`, "could not clear CI readiness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Env = fakeCIGH(t, "OPEN", tc.checks)
			sctx.Config.CITimeout = time.Minute
			writeErr := errors.New("readiness write failed")
			sctx.SetCIReadiness = func(bool, bool) error { return writeErr }
			var logs []string
			sctx.Log = func(message string) { logs = append(logs, message) }
			stop := errors.New("monitor reached next poll")
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return stop }}
			if _, err := step.Execute(sctx); !errors.Is(err, stop) {
				t.Fatalf("monitor error = %v, want next poll", err)
			}
			if !strings.Contains(strings.Join(logs, "\n"), "warning: "+tc.warning+": "+writeErr.Error()) {
				t.Fatalf("logs = %v, want readiness warning", logs)
			}
		})
	}
}

func TestCIStep_ReadinessClearFailureWithPendingHandoffKeepsIssueObservation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`)
	sctx.HandoffPending = func() bool { return true }
	sctx.Config.CITimeout = time.Minute
	writeErr := errors.New("readiness write failed")
	sctx.SetCIReadiness = func(bool, bool) error { return writeErr }
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("monitor error = %v, want issue observation despite readiness failure", err)
	}
	if outcome == nil {
		t.Fatal("monitor outcome = nil, want issue observation")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "warning: could not clear CI readiness: "+writeErr.Error()) {
		t.Fatalf("logs = %v, want readiness warning", logs)
	}
}

func TestCIStep_ReadinessWriteFailureRetriesOnNextPoll(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	green := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	sctx.Env = fakeCIGHSequence(t, "OPEN", []string{green, green})
	sctx.Config.CITimeout = time.Minute
	writeErr := errors.New("readiness write failed")
	var readinessWrites int
	sctx.SetCIReadiness = func(ready, declaredNoCI bool) error {
		readinessWrites++
		if readinessWrites <= 3 {
			if !ready || declaredNoCI {
				t.Fatalf("first readiness write = (%t, %t), want green readiness", ready, declaredNoCI)
			}
			return writeErr
		}
		if !ready || declaredNoCI {
			t.Fatalf("recovered readiness write = (%t, %t), want green readiness", ready, declaredNoCI)
		}
		return nil
	}
	polls := 0
	stop := errors.New("monitor reached next poll")
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		polls++
		if polls < 4 {
			return nil
		}
		return stop
	}}

	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }
	if _, err := step.Execute(sctx); !errors.Is(err, stop) {
		t.Fatalf("monitor error = %v, want next poll", err)
	}
	if readinessWrites != 4 {
		t.Fatalf("readiness writes = %d, want three failed writes plus recovery", readinessWrites)
	}
	if got := strings.Count(strings.Join(logs, "\n"), "warning: could not persist CI readiness:"); got != 1 {
		t.Fatalf("warning count = %d, want 1; logs: %v", got, logs)
	}
	if got := strings.Count(strings.Join(logs, "\n"), ciChecksPassedMsg); got != 1 {
		t.Fatalf("status count = %d, want 1; logs: %v", got, logs)
	}
}

func TestCIStep_MergeConflictDetected_ReturnsNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// All CI checks pass, but PR has merge conflicts
	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	// The timeout is an idle production budget, not the mechanism under test.
	// Leave enough room for race-enabled, process-saturated CI to start the fake
	// gh subprocesses before the mergeability response is observed.
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when merge conflict detected")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	foundConflict := false
	for _, f := range findings.Items {
		if strings.Contains(f.Description, "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict finding, got: %+v", findings.Items)
	}
}

func TestCIStep_MergeConflictAndCIFailure_FixPromptIncludesBoth(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Log = func(s string) {}

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	driveCI(t, step, sctx)

	if capturedPrompt == "" {
		t.Fatal("expected agent to be called")
	}
	if !strings.Contains(capturedPrompt, "merge conflict") {
		t.Errorf("expected prompt to mention merge conflict, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "test") {
		t.Errorf("expected prompt to mention failing check name, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_MergeConflictOnly_AutoFix(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// All checks pass, but merge conflict
	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	agentCalled := false
	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "conflict-fix.txt"), []byte("resolved"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	driveCI(t, step, sctx)

	if !agentCalled {
		t.Fatal("expected agent to be called to resolve merge conflict")
	}
	if strings.Contains(capturedPrompt, "You MUST produce file changes that fix the failing checks") {
		t.Fatalf("merge-conflict-only prompt should not require file changes for failing checks, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Rebase onto the base branch and resolve the merge conflicts") {
		t.Fatalf("expected merge-conflict-only prompt to focus on rebase flow, got:\n%s", capturedPrompt)
	}

	// Should log about merge conflict
	foundConflict := false
	for _, l := range logs {
		if strings.Contains(l, "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict log, got: %v", logs)
	}
}

func TestCIStep_MergeConflictAutoFixPromptUsesBaseBranchTip(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	// A receive into a bare repository may otherwise launch detached automatic
	// maintenance that races t.TempDir cleanup after the synchronous push exits.
	gitCmd(t, upstream, "config", "gc.auto", "0")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	featureHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "shared.txt")
	gitCmd(t, dir, "commit", "-m", "main update")
	mainTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			if err := os.WriteFile(filepath.Join(opts.CWD, "conflict-fix.txt"), []byte("resolved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, featureHead, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.DefaultBranch = "main"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	step := &CIStep{}
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	pr := &scm.PR{Number: "42", URL: prURL}
	_, err := step.autoFixCI(sctx, host, pr, ciTargetsFor(nil, true))
	if err != nil {
		t.Fatalf("auto-fix CI: %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected agent to receive a prompt")
	}
	if !strings.Contains(capturedPrompt, "base commit: "+mainTip) {
		t.Fatalf("expected prompt to use base branch tip %s, got:\n%s", mainTip, capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "base commit: "+baseSHA) {
		t.Fatalf("expected prompt to avoid merge-base %s, got:\n%s", baseSHA, capturedPrompt)
	}
}

func TestCIStep_AutoFixUsesExistingPRBaseAfterConfigChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "develop")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("develop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "develop")
	developTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "develop")

	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.PR.BaseBranch = "main"
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	// This test pins the ci.revalidate_repairs: true path, where the
	// repair is held locally until Review re-approves it.
	sctx.Config.CI.RevalidateRepairs = true
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42", BaseBranch: "develop"}

	var prompt string
	sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		return &agent.Result{}, nil
	}}
	sctx.Env = fakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "CONFLICTING")
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(skip)
	}
	if _, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor(nil, true)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "base commit: "+developTip) {
		t.Fatalf("prompt did not use existing PR base %s:\n%s", developTip, prompt)
	}
}
