package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
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

func TestCIMonitorReadinessChangeNotifiesConsumers(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	var changes [][2]bool
	sctx.CIReadinessChanged = func(ready, declaredNoCI bool) {
		changes = append(changes, [2]bool{ready, declaredNoCI})
	}

	logCIMonitorStatus(sctx, ciNoChecksPassedMsg, "")
	clearCIMonitorReady(sctx)

	want := [][2]bool{{true, true}, {false, false}}
	if len(changes) != len(want) {
		t.Fatalf("readiness changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Errorf("readiness change %d = %v, want %v", i, changes[i], want[i])
		}
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

// TestCIStep_EmptyChecksWithoutNoCIStaysNotReadyPastOldGracePeriod proves the
// PR 607 failure mode is closed: a generic empty forge response never becomes
// ready, even after the historical 60s grace period and longer timing windows.
func TestCIStep_EmptyChecksWithoutNoCIStaysNotReadyPastOldGracePeriod(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.NoCI = false

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	const oldGrace = 60 * time.Second
	step := &CIStep{
		pollIntervalOverride: 30 * time.Second,
		now:                  func() time.Time { return current },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			current = current.Add(interval)
			// Past the old 60s grace and well into multi-minute delayed registration.
			if current.Sub(started) > 3*time.Minute {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected continued waiting after empty checks, got %v", err)
	}
	if current.Sub(started) <= oldGrace {
		t.Fatalf("test did not advance past old grace period: elapsed %v", current.Sub(started))
	}
	for _, l := range logs {
		if l == cimonitor.NoChecksPassedMsg || l == cimonitor.ChecksPassedMsg {
			t.Fatalf("empty checks without no_ci must not emit ready marker, got logs: %v", logs)
		}
		if l == "no CI checks reported - still monitoring until merged or closed" {
			t.Fatalf("legacy empty-as-green marker must not be emitted, got logs: %v", logs)
		}
	}
	foundWaiting := false
	for _, l := range logs {
		if strings.Contains(l, "waiting for checks to register") {
			foundWaiting = true
			break
		}
	}
	if !foundWaiting {
		t.Fatalf("expected waiting-for-registration log, got: %v", logs)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("cimonitor must report not-ready for empty checks without no_ci, logs: %v", logs)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatalf("expected CI readiness unset without no_ci, got %v", *dbRun.CIReadyAt)
	}
}

// TestCIStep_EmptyChecksWithTrustedNoCIBecomesReady proves a positively
// declared no-CI repository with zero checks returns the selected
// all-checks-passed agent-facing result, with the declaration inspectable in
// the log line.
func TestCIStep_EmptyChecksWithTrustedNoCIBecomesReady(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second
	sctx.Config.NoCI = true

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
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected continued monitoring after declared no-CI ready, got %v", err)
	}
	found := false
	for _, l := range logs {
		if l == cimonitor.NoChecksPassedMsg {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected declared no_ci ready marker, got: %v", logs)
	}
	if !cimonitor.ChecksPassed(logs) {
		t.Fatal("declared no_ci with zero checks must be agent-facing ready")
	}
	if !cimonitor.DeclaredNoCI(logs) {
		t.Fatal("declared no_ci ready path must expose DeclaredNoCI evidence")
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("expected CI readiness persisted for declared no_ci")
	}
}

// TestCIStep_DelayedCheckRegistrationStaysNotReadyUntilGreen replays the
// delayed-registration path: empty forge results stay not-ready, pending
// checks stay not-ready, failures stay failures, and only all-green becomes
// ready.
func TestCIStep_DelayedCheckRegistrationStaysNotReadyUntilGreen(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[]`,
		`[]`,
		`[{"name":"e2e","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"e2e","state":"FAILURE","bucket":"fail","completedAt":"2026-07-30T08:06:01Z"}]`,
		`[{"name":"e2e","state":"SUCCESS","bucket":"pass","completedAt":"2026-07-30T08:10:00Z"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/607"
	ag := &mockAgent{name: "test"}
	// auto_fix.ci = 0 so the failure parks rather than auto-fixing; we only
	// care about readiness transitions across the delayed-registration path.
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.NoCI = false
	zero := 0
	sctx.Config.AutoFix.CI = zero

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	phase := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			phase++
			switch phase {
			case 1, 2:
				if cimonitor.ChecksPassed(logs) {
					t.Fatalf("phase %d empty checks must not be ready; logs=%v", phase, logs)
				}
				dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if dbRun.CIReadyAt != nil {
					t.Fatalf("phase %d must not persist readiness", phase)
				}
				return nil
			case 3:
				if cimonitor.ChecksPassed(logs) {
					t.Fatalf("pending checks must not be ready; logs=%v", logs)
				}
				foundRunning := false
				for _, l := range logs {
					if l == cimonitor.ChecksRunningMsg {
						foundRunning = true
						break
					}
				}
				if !foundRunning {
					t.Fatalf("expected running marker after delayed registration, logs=%v", logs)
				}
				return nil
			case 4:
				// Failure should park the step; Execute returns before another wait.
				return nil
			default:
				cancel()
				return ctx.Err()
			}
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected failure outcome without error, got err=%v outcome=%v", err, outcome)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected failure to park for approval, got %#v", outcome)
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("failed checks must not be ready; logs=%v", logs)
	}

	// Resume with green checks on a fresh step instance (same empty→pending→green
	// contract after a fix round). Prove green becomes ready.
	logs = nil
	env = fakeCIGH(t, "OPEN", `[{"name":"e2e","state":"SUCCESS","bucket":"pass"}]`)
	sctx.Env = env
	sctx.Ctx = context.Background()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	greenStep := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err = greenStep.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected continued monitoring after green, got %v", err)
	}
	if !cimonitor.ChecksPassed(logs) {
		t.Fatalf("all-green must be ready; logs=%v", logs)
	}
	if cimonitor.DeclaredNoCI(logs) {
		t.Fatal("all-green path must not report DeclaredNoCI")
	}
}

// TestCIStep_DeclaredNoCIWithUnexpectedChecksHonorsThem proves no_ci never
// waives registered pending or failing checks.
func TestCIStep_DeclaredNoCIWithUnexpectedChecksHonorsThem(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"surprise","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"surprise","state":"FAILURE","bucket":"fail","completedAt":"2026-07-30T08:06:01Z"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/99"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.NoCI = true
	zero := 0
	sctx.Config.AutoFix.CI = zero

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	phase := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			phase++
			if phase == 1 {
				if cimonitor.ChecksPassed(logs) {
					t.Fatalf("pending unexpected checks must not be ready under no_ci; logs=%v", logs)
				}
				return nil
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected failure outcome, got err=%v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected failing unexpected checks to park, got %#v", outcome)
	}
	for _, l := range logs {
		if l == cimonitor.NoChecksPassedMsg {
			t.Fatalf("no_ci ready marker must not fire while checks are registered; logs=%v", logs)
		}
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("failing checks under no_ci must not be ready; logs=%v", logs)
	}
}

func TestCIStep_NonEmptyPassingChecksContinueMonitoring(t *testing.T) {
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
