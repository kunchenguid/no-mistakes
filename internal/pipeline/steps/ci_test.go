package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_PendingChecksUseAdaptivePollIntervals(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 20 * time.Minute

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started
	var waits []time.Duration

	step := &CIStep{
		now: func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			switch len(waits) {
			case 1:
				current = started.Add(5 * time.Minute)
			case 2:
				current = started.Add(15 * time.Minute)
			case 3:
				current = current.Add(interval)
			default:
				t.Fatalf("unexpected extra poll wait: %v", interval)
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected pending checks to exit cleanly once checks pass")
	}

	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("wait count = %d, want %d (%v)", len(waits), len(want), waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("wait %d = %v, want %v (all waits: %v)", i, waits[i], want[i], waits)
		}
	}
}

func TestCIStep_UsesStepEnvForCLIStartupChecks(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	hiddenPath := t.TempDir()
	t.Setenv("PATH", hiddenPath)

	env := fakeCIGH(t, "MERGED", "[]")
	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected merged PR to exit cleanly")
	}
	for _, logLine := range logs {
		if strings.Contains(logLine, "gh CLI is not installed") || strings.Contains(logLine, "gh CLI is not authenticated") {
			t.Fatalf("expected startup checks to use StepContext env, got logs: %v", logs)
		}
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "PR has been merged") {
		t.Fatalf("expected CI monitoring to reach PR state check, got logs: %v", logs)
	}
}

func TestCIStep_InvalidPRURLReturnsError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42/files"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	step := &CIStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error for invalid PR URL")
	}
	if !strings.Contains(err.Error(), "extract PR number") {
		t.Fatalf("expected extract PR number context, got %v", err)
	}
	if !strings.Contains(err.Error(), `invalid PR number "files"`) {
		t.Fatalf("expected invalid PR number detail, got %v", err)
	}
}

func TestCIStep_ContextCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/1"
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	sctx.Ctx = ctx

	step := &CIStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCIStep_Execute_FixMode_RemoteAlreadyUpdatedDoesNotReturnManualIntervention(t *testing.T) {
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
	originalHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	os.WriteFile(filepath.Join(dir, "resolved.txt"), []byte("resolved"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "resolve conflict")
	advancedHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "--force-with-lease", "origin", "HEAD:refs/heads/feature")

	checksJSON := `[{"name":"build","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "MERGEABLE")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, originalHeadSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Fixing = true
	sctx.Config.CITimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected polling to continue after head reconciliation, got %v", err)
	}

	if sctx.Run.HeadSHA != advancedHeadSHA {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, advancedHeadSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != advancedHeadSHA {
		t.Fatalf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, advancedHeadSHA)
	}
}

func TestCIStep_PRMergedExitsEarly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "MERGED", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for merged PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "merged") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'merged' in logs, got: %v", logs)
	}
}

func TestCIStep_PRClosedExitsEarly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "CLOSED", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for closed PR")
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "closed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'closed' in logs, got: %v", logs)
	}
}

func TestCIStep_GetCIChecksNoChecksReported(t *testing.T) {
	t.Parallel()
	env := fakeCIGHNoChecks(t)

	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	sctx.Env = env

	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("expected no error when gh reports no checks, got: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected no checks, got: %#v", checks)
	}
}

func TestCIStep_AllChecksPassingExitsCleanly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when CI checks are already passing")
	}
	if pollCount != 1 {
		t.Fatalf("expected one poll wait while CI checks were pending, got %d", pollCount)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected clean-exit CI log, got: %v", logs)
	}
}

func TestCIStep_EmptyChecksWaitsDuringGracePeriod(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Fake gh returns OPEN state, empty checks, no comments
	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started
	var waits []time.Duration

	step := &CIStep{
		checksGracePeriod:    200 * time.Millisecond,
		pollIntervalOverride: 75 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			current = current.Add(interval)
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed")
	}
	if elapsed := current.Sub(started); elapsed < 200*time.Millisecond {
		t.Errorf("CI exited in %v, expected to wait at least 200ms grace period", elapsed)
	}
	if len(waits) != 3 {
		t.Fatalf("expected 3 grace-period waits, got %v", waits)
	}
	for _, interval := range waits {
		if interval != 75*time.Millisecond {
			t.Fatalf("expected 75ms waits during grace period, got %v", waits)
		}
	}
	// Should exit via grace period expiry, not CI timeout
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			t.Fatal("expected exit via grace period expiry, not CI timeout")
		}
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "CI monitoring complete") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'CI monitoring complete' log, got: %v", logs)
	}
}

func TestCIStep_LogsWaitingForChecksDuringGracePeriod(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	current := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 10 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			current = current.Add(interval)
			return nil
		},
	}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "waiting for checks to register") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected grace-period waiting log, got: %v", logs)
	}
}

func TestCIStep_NonEmptyPassingChecksExitImmediately(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	// Even with a long grace period, non-empty passing checks should exit immediately
	step := &CIStep{checksGracePeriod: 10 * time.Second}
	started := time.Now()
	outcome, err := step.Execute(sctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed")
	}
	if elapsed > 10*time.Second {
		t.Errorf("non-empty passing checks should exit quickly, took %v", elapsed)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'all CI checks passed' log, got: %v", logs)
	}
}
