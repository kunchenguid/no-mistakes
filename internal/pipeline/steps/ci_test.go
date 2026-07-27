package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

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
				cancel()
				return ctx.Err()
			default:
				t.Fatalf("unexpected extra poll wait: %v", interval)
			}
			return nil
		},
	}

	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after observing adaptive waits, got %v", err)
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

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	hiddenPath := fakeCLIBinDir(t)
	linkTestBinary(t, hiddenPath, "git")
	t.Setenv("FAKE_CLI_MODE", "git-passthrough")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("PATH", hiddenPath)

	env := fakeCIGH(t, "MERGED", "[]")
	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.PRState == nil || *dbRun.PRState != "merged" || dbRun.PRStateObservedAt == nil {
		t.Fatalf("structured PR lifecycle = %#v", dbRun)
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

func TestCIStep_AllChecksPassingKeepsMonitoringOpenPR(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 1 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 2 {
		t.Fatalf("expected one pending wait plus one healthy monitoring wait, got %d", pollCount)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring CI log, got: %v", logs)
	}
}

func TestCIStep_CIWarningAllowsChecksPassedToBeReannounced(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`not-json`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits++
			if waits == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue, got %v", err)
	}

	passedLogs := 0
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			passedLogs++
		}
	}
	if passedLogs != 2 {
		t.Fatalf("expected checks-passed status before and after CI warning, got %d logs: %v", passedLogs, logs)
	}
}

func TestCIStep_CIWarningClearsPersistedReadiness(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`not-json`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits++
			if waits == 1 {
				dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if dbRun.CIReadyAt == nil {
					t.Fatal("expected passing checks to persist CI readiness")
				}
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue, got %v", err)
	}

	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatalf("expected CI warning to clear readiness, got %v", *dbRun.CIReadyAt)
	}
}

func TestCIStep_UncertainProviderStateClearsPersistedReadiness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  func(t *testing.T) []string
	}{
		{
			name: "pr_state_error",
			env: func(t *testing.T) []string {
				return fakeCIGHStateError(t, "provider unavailable", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
			},
		},
		{
			name: "mergeability_unknown",
			env: func(t *testing.T) []string {
				return fakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "UNKNOWN")
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)

			prURL := "https://github.com/test/repo/pull/42"
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = tt.env(t)
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = 10 * time.Second
			if err := sctx.DB.SetRunCIReady(sctx.Run.ID, true); err != nil {
				t.Fatal(err)
			}

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
				t.Fatalf("expected open PR monitoring to continue, got %v", err)
			}

			dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if dbRun.CIReadyAt != nil {
				t.Fatalf("expected provider uncertainty to clear readiness, got %v", *dbRun.CIReadyAt)
			}
		})
	}
}

func TestCIStep_OpenPRKeepsMonitoringAfterChecksPass(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 1 {
		t.Fatalf("poll count = %d, want 1", pollCount)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		checksGracePeriod:    200 * time.Millisecond,
		pollIntervalOverride: 75 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			if current.Sub(started) >= 200*time.Millisecond {
				cancel()
				return ctx.Err()
			}
			current = current.Add(interval)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after grace-period monitoring continued, got %v", err)
	}
	if elapsed := current.Sub(started); elapsed < 200*time.Millisecond {
		t.Errorf("CI exited in %v, expected to wait at least 200ms grace period", elapsed)
	}
	if len(waits) != 4 {
		t.Fatalf("expected 3 grace-period waits plus one continued-monitoring wait, got %v", waits)
	}
	for _, interval := range waits[:3] {
		if interval != 75*time.Millisecond {
			t.Fatalf("expected 75ms waits during grace period, got %v", waits)
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			t.Fatal("expected cancellation before CI timeout")
		}
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "no CI checks reported - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring log after grace period, got: %v", logs)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	current := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	step := &CIStep{
		checksGracePeriod:    50 * time.Millisecond,
		pollIntervalOverride: 10 * time.Millisecond,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after first grace-period wait, got %v", err)
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

func TestCIStep_NonEmptyPassingChecksSkipGracePeriodAndContinueMonitoring(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		checksGracePeriod: 10 * time.Second,
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			cancel()
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected open PR monitoring to continue after passing checks, got %v", err)
	}
	if pollCount != 1 {
		t.Fatalf("expected one healthy monitoring wait, got %d", pollCount)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring pass log, got: %v", logs)
	}
}

// TestCIStep_BaseBranchAdvanceRearmsTimeout verifies the monitor survives past
// its original idle timeout when the base branch advances mid-monitoring: each
// advance re-arms the deadline so a long-held green PR keeps getting watched
// and rebased instead of being silently dropped.
func TestCIStep_BaseBranchAdvanceRearmsTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			if tipCalls == 1 {
				return "sha-old", true
			}
			return "sha-new", true
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			switch pollCount {
			case 1:
				current = started.Add(8 * time.Second)
			case 2:
				// 16s since start is past the 10s timeout, but the base advanced
				// at 8s and re-armed the deadline, so monitoring must continue.
				current = started.Add(16 * time.Second)
			default:
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue past the original timeout after re-arm, got %v", err)
	}

	rearmed := false
	for _, l := range logs {
		if strings.Contains(l, "re-arming CI monitor timeout") {
			rearmed = true
		}
		if strings.Contains(l, "CI timeout reached") {
			t.Fatalf("monitor timed out despite a base-branch advance re-arm; logs: %v", logs)
		}
	}
	if !rearmed {
		t.Fatalf("expected a re-arm log after the base branch advanced; logs: %v", logs)
	}
}

// TestCIStep_StableBaseStillTimesOut verifies the timeout still fires normally
// for a PR whose base branch never moves, preserving the bounded-monitoring
// behavior for genuinely idle/abandoned PRs.
func TestCIStep_StableBaseStillTimesOut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { return "sha-stable", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			current = started.Add(12 * time.Second)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'CI timeout reached' log for a stable base, got: %v", logs)
	}
}

func TestCIStep_UnresolvedFallbackBaseTipDoesNotRearmTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			switch tipCalls {
			case 1:
				return "sha-remote", true
			case 2:
				return baseSHA, false
			default:
				return "sha-remote", true
			}
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			switch pollCount {
			case 1:
				current = started.Add(8 * time.Second)
			case 2:
				current = started.Add(16 * time.Second)
			default:
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	for _, l := range logs {
		if strings.Contains(l, "re-arming CI monitor timeout") {
			t.Fatalf("fallback base SHA must not re-arm timeout; logs: %v", logs)
		}
	}
}

func TestCIStep_ExpiredTimeoutSkipsBaseTipResolver(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	tipCalls := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) {
			tipCalls++
			if tipCalls > 1 {
				t.Fatal("base tip resolver should not run after timeout expiry")
			}
			return "sha-stable", true
		},
		waitForNextPoll: func(context.Context, time.Duration) error {
			current = started.Add(11 * time.Second)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected timeout outcome, got error %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected timeout to surface a needs-approval outcome, got %+v", outcome)
	}
	if tipCalls != 1 {
		t.Fatalf("base tip resolver calls = %d, want 1", tipCalls)
	}
}

func TestCIStep_BaseTipResolverDeadlineIsBoundedByRemainingTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	step := &CIStep{
		now: func() time.Time { return current },
		baseBranchTip: func(ctx context.Context) (string, bool) {
			tipCalls++
			if tipCalls == 1 {
				return "sha-stable", true
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected base tip resolver context to have a deadline")
			}
			if remaining := time.Until(deadline); remaining > 2*time.Second {
				t.Fatalf("base tip resolver deadline = %v from now, want no more than 2s", remaining)
			}
			return "sha-stable", true
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			if tipCalls == 1 {
				current = started.Add(8 * time.Second)
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after deadline inspection, got %v", err)
	}
}

// TestCIStep_UnlimitedTimeoutNeverExpires verifies that an unlimited timeout
// (ci_timeout: "unlimited" / non-positive) makes the monitor watch until the
// PR merges or closes, never self-terminating, and skips base-tip polling.
func TestCIStep_UnlimitedTimeoutNeverExpires(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = config.CITimeoutUnlimited

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	tipCalls := 0
	pollCount := 0
	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { tipCalls++; return "sha", true },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount >= 2 {
				cancel()
				return ctx.Err()
			}
			// Jump far past any finite default timeout to prove it never fires.
			current = started.Add(30 * 24 * time.Hour)
			return nil
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected unlimited monitoring to continue indefinitely, got %v", err)
	}
	if tipCalls != 0 {
		t.Fatalf("expected no base-tip polling under an unlimited timeout, got %d calls", tipCalls)
	}
	timeoutLog, noTimeoutLog := false, false
	for _, l := range logs {
		if strings.Contains(l, "CI timeout reached") {
			timeoutLog = true
		}
		if strings.Contains(l, "no timeout, until merged or closed") {
			noTimeoutLog = true
		}
	}
	if timeoutLog {
		t.Fatalf("unlimited monitor must not time out; logs: %v", logs)
	}
	if !noTimeoutLog {
		t.Fatalf("expected the no-timeout monitoring log, got: %v", logs)
	}
}

// setupCIRerunRepo builds a worktree whose feature branch is published on a
// local bare upstream, so the CI step can verify the published head with
// ls-remote exactly as it does in production.
func setupCIRerunRepo(t *testing.T) (dir, upstream, baseSHA, headSHA string) {
	t.Helper()

	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	return dir, upstream, baseSHA, headSHA
}

func ghLog(t *testing.T, logFile string) string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read gh log: %v", err)
	}
	return string(data)
}

// A check the provider cancelled is not a verdict on the code: it is re-run for
// the same commit, the fix agent is never involved, and the monitor reports
// checks as running again rather than leaving an earlier state to look current.
func TestCIStep_CancelledCheckIsRerunBeforeEscalating(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue after the rerun, got %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round for a cancelled check, got %d", len(ag.calls))
	}
	if !strings.Contains(ghLog(t, logFile), "run rerun --job 901") {
		t.Fatalf("expected the rerun to target the check's job, gh log:\n%s", ghLog(t, logFile))
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one, gh log:\n%s", got, ghLog(t, logFile))
	}

	rerunIndex, runningIndex, passedIndex := -1, -1, -1
	for i, l := range logs {
		switch {
		case strings.Contains(l, "re-running CI check test (1/1)"):
			rerunIndex = i
		case l == ciChecksRunningMsg:
			runningIndex = i
		case l == ciChecksPassedMsg:
			passedIndex = i
		case strings.Contains(l, "auto-fixing"):
			t.Fatalf("cancelled check escalated to the fix agent; logs: %v", logs)
		}
	}
	if rerunIndex < 0 {
		t.Fatalf("expected the rerun to be reported as its own event, got: %v", logs)
	}
	// The TUI and axi read monitoring state back out of these lines, so the poll
	// that re-runs a check must report checks as running: a cancelled check never
	// counted as failing, so nothing else clears an earlier passed-checks line.
	if runningIndex < rerunIndex || passedIndex < runningIndex {
		t.Fatalf("expected rerun, then checks running, then checks passed, got: %v", logs)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("expected CI readiness once the re-run check passed")
	}

	t.Log("CI step log:")
	for _, l := range logs {
		t.Logf("    %s", l)
	}
	t.Log("gh commands the monitor issued:")
	for _, l := range strings.Split(strings.TrimSpace(ghLog(t, logFile)), "\n") {
		t.Logf("    gh %s", l)
	}
	t.Logf("fix-agent rounds consumed: %d", len(ag.calls))
}

// A check that comes back cancelled after its rerun is unresolved, not green: it
// must reach the same gate a failing check does, and it must never produce a
// ready-to-merge signal.
func TestCIStep_CancelledCheckStaysUnresolvedAfterItsBudget(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	cancelled := `[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`
	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{cancelled, cancelled, cancelled}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		// Bounded: a regression that never escalates must fail the test rather
		// than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a check that stayed cancelled to escalate")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "test") {
		t.Fatalf("findings = %+v, want the cancelled check named", findings.Items)
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one, gh log:\n%s", got, ghLog(t, logFile))
	}
	for _, l := range logs {
		if l == ciChecksPassedMsg {
			t.Fatalf("a cancelled check must never report checks passed; logs: %v", logs)
		}
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatal("a PR whose only check is cancelled must not be marked ready to merge")
	}

	t.Log("CI step log:")
	for _, l := range logs {
		t.Logf("    %s", l)
	}
	t.Logf("outcome: needs_approval=%v summary=%q finding=%q", outcome.NeedsApproval, findings.Summary, findings.Items[0].Description)
	t.Logf("rerun requests: %d", strings.Count(ghLog(t, logFile), "run rerun"))
	t.Log("ci ready: not set")
}

// Check names are not unique on a PR, and same-named checks share one budget
// key, so one poll must not spend that budget once per check.
func TestCIStep_SameNamedCancelledChecksShareOneRerunBudget(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"build","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"},{"name":"build","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/901/job/902"}]`,
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"},{"name":"build","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/901/job/902"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 2 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	step.Execute(sctx)

	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one for a budget of one, gh log:\n%s", got, ghLog(t, logFile))
	}
	for _, l := range logs {
		if strings.Contains(l, "re-running CI check build") && !strings.Contains(l, "(1/1)") {
			t.Fatalf("rerun reported outside its budget: %q", l)
		}
	}
}

// A genuine failure must reach the fix agent on its first failure. A cancelled
// sibling in the same poll must not buy it another CI cycle, because no rerun
// can clear the genuine failure.
func TestCIStep_GenuineCheckFailureEscalatesOnFirstFailure(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"lint","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"},{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/902"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCalls := 0
	step := &CIStep{
		// Bounded: a regression that defers this escalation must fail the test
		// rather than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCalls++
			if pollCalls >= 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a genuine failure to escalate immediately")
	}
	if pollCalls != 0 {
		t.Fatalf("genuine failure waited %d extra polls before escalating, want 0", pollCalls)
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("a genuine failure must never be re-run, gh log:\n%s", ghLog(t, logFile))
	}
}

// A merge conflict is the one CI-step issue no rerun can ever clear, so a
// cancelled check must not defer it by a whole CI cycle.
func TestCIStep_MergeConflictEscalatesWithoutRerunningChecks(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
	}, "CONFLICTING", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCalls := 0
	step := &CIStep{
		// Bounded: a regression that defers this escalation must fail the test
		// rather than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCalls++
			if pollCalls >= 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the merge conflict to escalate immediately")
	}
	if pollCalls != 0 {
		t.Fatalf("merge conflict waited %d extra polls before escalating, want 0", pollCalls)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	foundConflict := false
	for _, item := range findings.Items {
		if strings.Contains(item.Description, "merge conflict") {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Fatalf("findings = %+v, want the merge conflict reported", findings.Items)
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("no rerun can clear a merge conflict, gh log:\n%s", ghLog(t, logFile))
	}
}

// A job that exceeded its own timeout is the provider reporting the job, not
// itself: it escalates on the first failure like any other genuine failure.
func TestCIStep_TimedOutCheckEscalatesWithoutRerunning(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"test","state":"TIMED_OUT","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCalls := 0
	step := &CIStep{
		// Bounded: a regression that defers this escalation must fail the test
		// rather than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCalls++
			if pollCalls >= 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a timed-out check to escalate")
	}
	if pollCalls != 0 {
		t.Fatalf("timed-out check waited %d extra polls before escalating, want 0", pollCalls)
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("a timed-out job must not be re-run by default, gh log:\n%s", ghLog(t, logFile))
	}
}

// Reruns are opt-out: with the budget at 0 nothing is re-run and a cancelled
// check keeps the exact behavior it had before this policy existed.
func TestCIStep_ZeroRerunBudgetRestoresPreviousBehavior(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: 0}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue, got %v", err)
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("reruns are disabled, gh log:\n%s", ghLog(t, logFile))
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round, got %d", len(ag.calls))
	}
	// Before this policy a cancelled check was neither failing nor pending, so
	// the monitor reported the PR as green. With the budget at 0 that is exactly
	// what still happens.
	sawPassed := false
	for _, l := range logs {
		if l == ciChecksPassedMsg {
			sawPassed = true
		}
	}
	if !sawPassed {
		t.Fatalf("expected the pre-policy behavior for a cancelled check, got: %v", logs)
	}
}

// A rerun re-runs whatever commit the branch now points at, so it is only
// meaningful while that is still the commit this run delivered. When the
// published head has moved, the step terminates with the expected and observed
// commits instead of certifying a revision it never produced.
func TestCIStep_MovedPublishedHeadTerminatesInsteadOfRerunning(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	// Someone else advances the published branch out of band.
	os.WriteFile(filepath.Join(dir, "out-of-band.txt"), []byte("out of band"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "out of band commit")
	movedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		// Bounded: a regression that never escalates must fail the test rather
		// than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a moved published head to terminate the step")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one head-mismatch finding", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, headSHA) || !strings.Contains(findings.Items[0].Description, movedSHA) {
		t.Fatalf("finding %q must name both the expected head %s and the observed head %s", findings.Items[0].Description, headSHA, movedSHA)
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("a rerun against a different head is meaningless and must not be requested, gh log:\n%s", ghLog(t, logFile))
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round, got %d", len(ag.calls))
	}
	mismatchLogged := false
	for _, l := range logs {
		if strings.Contains(l, "published branch head moved") {
			mismatchLogged = true
		}
	}
	if !mismatchLogged {
		t.Fatalf("expected the head mismatch to be reported, got: %v", logs)
	}
}

// A provider that refuses the rerun must not stall the run: the budget is spent
// on the attempt, so the check escalates on the next poll instead of asking for
// the same rerun forever.
func TestCIStep_RefusedRerunSpendsBudgetAndEscalates(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	cancelled := `[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`
	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{cancelled, cancelled}, "", "HTTP 403: Unable to retry this workflow run")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		// Bounded: a regression that never escalates must fail the test rather
		// than keep monitoring until the CI timeout.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the check to escalate after the refused rerun")
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one even though it failed, gh log:\n%s", got, ghLog(t, logFile))
	}
	warned := false
	for _, l := range logs {
		if strings.Contains(l, "could not re-run transient CI check test") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected the refused rerun to be surfaced, got: %v", logs)
	}
}
