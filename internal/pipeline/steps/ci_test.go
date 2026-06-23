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
