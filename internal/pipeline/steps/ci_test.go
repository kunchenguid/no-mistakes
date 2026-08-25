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
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
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
	outcome, err := step.Execute(sctx)
	assertCIRestartsValidation(t, outcome, err)

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

func TestCIStep_FailedHeadWorkflowRunPreventsChecksPassed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", `[
		{"name":"clippy","state":"SUCCESS","bucket":"pass"},
		{"name":"request-owner-review","state":"SUCCESS","bucket":"pass"}
	]`)
	env = append(env, `FAKE_CLI_WORKFLOW_RUNS=[{
		"id":101,
		"name":"workflow-validation",
		"status":"completed",
		"conclusion":"failure",
		"updated_at":"2026-07-30T12:34:56Z"
	}]`)

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.AutoFix.CI = 0

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(context.Context, time.Duration) error {
			return errors.New("unexpected monitor poll after failed head workflow")
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v; logs: %v", err, logs)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("Execute() outcome = %+v, want failing CI approval gate", outcome)
	}
	for _, log := range logs {
		if strings.Contains(log, ciChecksPassedMsg) {
			t.Fatalf("failed head workflow was reported as checks-passed; logs: %v", logs)
		}
	}
	if !strings.Contains(outcome.Findings, "workflow-validation") {
		t.Fatalf("findings = %s, want failed workflow-validation run", outcome.Findings)
	}
	t.Logf("green PR rollup plus failed exact-head workflow produced approval gate: %s", outcome.Findings)
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
	// This test owns termination through the cancelled context. Check discovery
	// shells out several times per poll, so a short wall-clock timeout makes the
	// assertion depend on runner speed (especially under -race and on Windows).
	sctx.Config.CITimeout = config.CITimeoutUnlimited

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

func TestCIStep_PersistentCheckReadFailureParksAtAskUser(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Every poll fails to read checks (e.g. gh < v2.50 rejects `pr checks --json`).
	// The first few are tolerated as transient warnings, but a persistent streak
	// must park at an ask-user gate instead of spinning to ci_timeout.
	var checksSequence []string
	for i := 0; i < consecutiveCheckErrorLimit+3; i++ {
		checksSequence = append(checksSequence, `not-json`)
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	// Six consecutive failed polls must park before the timeout. Each poll
	// spawns three fake-gh subprocesses (~0.8s each under -race), so reaching
	// consecutiveCheckErrorLimit takes ~14s on a fast machine; give the loop
	// ample headroom so the timeout never races the streak.
	sctx.Config.CITimeout = 60 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		// No sleep between polls so the consecutive-failure streak accumulates
		// quickly instead of wedging on the 30s poll, and a stable base tip so
		// each poll does not pay a real git fetch (slow under -race on loaded CI).
		baseBranchTip:   func(context.Context) (string, bool) { return baseSHA, true },
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error { return nil },
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected ask-user approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected persistent check-read failures to park at an approval gate")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly one ask-user finding", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
	if !strings.Contains(findings.Items[0].Description, "pr checks --json") || !strings.Contains(findings.Items[0].Description, "2.50") {
		t.Fatalf("finding %q must explain the gh version/flag cause", findings.Items[0].Description)
	}
	parked := 0
	for _, l := range logs {
		if strings.Contains(l, "parking for a decision") {
			parked++
		}
	}
	if parked != 1 {
		t.Fatalf("expected one parking log line, got %d: %v", parked, logs)
	}
}

func TestCIStep_CheckReadFailureCounterResetsAfterSuccessfulRead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Five read failures, a green read, then one more failure: the success must
	// reset the consecutive counter, or the sixth cumulative error would wrongly
	// park a run whose failures were only transient blips.
	checksSequence := []string{
		`not-json`, `not-json`, `not-json`, `not-json`, `not-json`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`not-json`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 60 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	waits := 0
	step := &CIStep{
		baseBranchTip: func(context.Context) (string, bool) { return baseSHA, true },
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			waits++
			if waits == 8 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err == nil && outcome != nil && outcome.NeedsApproval {
		t.Fatalf("a successful read must reset the consecutive failure counter; parked after transient failures: %s", outcome.Findings)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue past the reset, got outcome=%v err=%v", outcome, err)
	}
}

// TestCICheckReadFailureOutcome_ProviderNeutral guards the parked finding text:
// it must give provider-agnostic remediation for every supported SCM, not an
// unconditional instruction to install or upgrade `gh`, which is GitHub-only.
func TestCICheckReadFailureOutcome_ProviderNeutral(t *testing.T) {
	t.Parallel()
	outcome := ciCheckReadFailureOutcome(errors.New("glab mr checks: failed to read checks"))
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly one finding", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
	desc := findings.Items[0].Description
	if !strings.Contains(desc, "provider CLI or credentials") {
		t.Fatalf("finding %q must give provider-neutral remediation, not a GitHub-only one", desc)
	}
	// The old text instructed verifying gh unconditionally ("verify gh supports
	// 'pr checks --json'") even for GitLab/Bitbucket/Azure errors. gh may only be
	// named as the conditional GitHub-specific clause, never the general remedy.
	if strings.Contains(desc, "verify gh supports") {
		t.Fatalf("finding %q must not instruct verifying gh for a non-GitHub provider", desc)
	}
	// The underlying provider error must survive into the finding.
	if !strings.Contains(desc, "glab mr checks: failed to read checks") {
		t.Fatalf("finding %q must include the underlying provider error", desc)
	}
	// And the GitHub-specific diagnostic is still present for gh-style errors.
	if !strings.Contains(desc, "pr checks --json") || !strings.Contains(desc, "2.50") {
		t.Fatalf("finding %q must keep the GitHub gh version/flag diagnostic", desc)
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

// setupCIRerunRepo builds a worktree whose feature branch is published on a
// local bare origin, so the CI step can verify the published head with
// ls-remote exactly as it does in production. The returned upstream URL remains
// a GitHub URL because it is also the SCM provider's repository identity.
func setupCIRerunRepo(t *testing.T) (dir, upstreamURL, baseSHA, headSHA string) {
	t.Helper()

	upstream := t.TempDir()
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

	return dir, "https://github.com/test/repo", baseSHA, headSHA
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
		t.Fatal("expected CI readiness once the same-head rerun passed")
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

// `gh run rerun` returns as soon as the provider accepts the request, while the
// new attempt replaces the cancelled check in the status rollup asynchronously.
// A poll that still reads the pre-rerun outcome must keep waiting: parking there
// would escalate the very check this run just re-ran, and asking again would
// bill the repository a duplicate workflow run.
func TestCIStep_LaggingRerunRollupKeepsWaitingForTheRepublishedCheck(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	// The identical completedAt is what makes the second poll a stale read of
	// the same cancellation rather than the re-run job ending cancelled again.
	cancelled := `[{"name":"test","state":"CANCELLED","bucket":"cancel","completedAt":"2026-07-26T12:00:00Z","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`
	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{
		cancelled,
		cancelled,
		`[{"name":"test","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","state":"SUCCESS","bucket":"pass","completedAt":"2026-07-26T12:06:00Z","link":"https://github.com/test/repo/actions/runs/900/job/902"}]`,
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
			if polls >= 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue while the rollup caught up, got outcome %+v err %v", outcome, err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round while the rerun was still publishing, got %d", len(ag.calls))
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one across the unrefreshed polls, gh log:\n%s", got, ghLog(t, logFile))
	}
	for _, l := range logs {
		if strings.Contains(l, "auto-fixing") || strings.Contains(l, "manual intervention") {
			t.Fatalf("a check whose rerun had not published yet escalated; logs: %v", logs)
		}
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("expected CI readiness once the re-run check reported success")
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

// A cancellation is never a verdict on the code, so a check that stayed
// cancelled after its rerun must not be handed to the fix agent even with
// auto_fix.ci at its default: there is nothing to repair, and the round would
// let an agent edit code the provider never tested.
func TestCIStep_UnresolvedCancelledCheckNeverEntersTheAutoFixLoop(t *testing.T) {
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
		t.Fatal("expected a check that stayed cancelled to park for a decision")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("cancelled check consumed %d fix-agent rounds, want 0; logs: %v", len(ag.calls), logs)
	}
	for _, l := range logs {
		if strings.Contains(l, "auto-fixing") {
			t.Fatalf("cancelled check entered the auto-fix loop; logs: %v", logs)
		}
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "test") {
		t.Fatalf("findings = %+v, want the cancelled check named", findings.Items)
	}
	// ask-user keeps a later fix loop from picking the finding up either.
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", findings.Items[0].Action, types.ActionAskUser)
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun"); got != 1 {
		t.Fatalf("rerun requests = %d, want exactly one, gh log:\n%s", got, ghLog(t, logFile))
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatal("a PR whose only check is cancelled must not be marked ready to merge")
	}
}

// A rerun cannot certify a commit this run never delivered, and the run must not
// leave an earlier ready-to-merge signal behind when it parks for that reason.
func TestCIStep_MovedPublishedHeadClearsCIReadiness(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	os.WriteFile(filepath.Join(dir, "out-of-band.txt"), []byte("out of band"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "out of band commit")
	gitCmd(t, dir, "push", "origin", "feature")

	// The first poll is green, so the run records CI readiness before the
	// cancelled check on the second poll reaches the head check.
	env, _ := fakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
		`[{"name":"test","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
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
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatal("a run parked because the published head moved must not report ci_ready")
	}
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

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

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

	t.Log("CI step log:")
	for _, l := range logs {
		t.Logf("    %s", l)
	}
	t.Log("gh commands the monitor issued:")
	for _, l := range strings.Split(strings.TrimSpace(ghLog(t, logFile)), "\n") {
		t.Logf("    gh %s", l)
	}
	t.Logf("extra polls waited before escalating: %d", pollCalls)
	t.Logf("outcome: needs_approval=%v findings=%s", outcome.NeedsApproval, outcome.Findings)
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

// Reruns are opt-out: with the budget at 0 nothing is re-run. The cancelled
// check cannot make the PR look ready, and because no rerun is coming it is
// also the run's final word on that check, so it escalates rather than being
// waited on.
func TestCIStep_ZeroRerunBudgetEscalatesCancelledCheckWithoutMakingItReady(t *testing.T) {
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
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: 0}

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
		t.Fatal("expected a cancelled check with no rerun budget to escalate")
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("reruns are disabled, gh log:\n%s", ghLog(t, logFile))
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round, got %d", len(ag.calls))
	}
	for _, l := range logs {
		if l == ciChecksPassedMsg {
			t.Fatalf("a cancelled check must not report checks passed; logs: %v", logs)
		}
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatal("a PR whose only check is cancelled must not be marked ready to merge")
	}
}

// Incident replay (firstmate PR 1495, 2026-08-02): the pipeline pushed its fix
// commits, GitHub ran the resulting workflow, every job finished, and one of
// them ended CANCELLED (GitHub reports a job that exceeds its timeout-minutes
// that way) while the rest passed. The CI monitor logged "CI checks running,
// waiting for results..." and kept polling that already-final rollup for the
// rest of the run's idle timeout - about 30 minutes before a manual abort.
//
// A cancelled check is terminal: the provider published a conclusion and will
// not publish another one on its own. With no rerun authorized there is
// nothing left for this run to wait for, so the poll must produce a decision.
// The regression arrived in #628, which started counting every non
// pass/fail/skip bucket as "pending" to stop empty and unknown check states
// from reading green, and thereby routed a terminal cancellation into the
// wait-for-more-results branch it can never leave.
func TestCIStep_CancelledCheckAmongPassingChecksEscalatesInsteadOfPollingForever(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	// The long-running job is still in progress on the first poll, exactly as
	// it was in the incident, so the monitor is right to wait there. It then
	// completes as CANCELLED and the rollup stops changing.
	running := `[{"name":"Repo invariants","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Lint shell scripts","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Behavior portable serial","state":"IN_PROGRESS","bucket":"pending"}]`
	settled := `[{"name":"Repo invariants","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Lint shell scripts","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Behavior portable serial","state":"CANCELLED","bucket":"cancel","completedAt":"2026-08-02T07:54:14Z","link":"https://github.com/test/repo/actions/runs/30738052151/job/91470340751"}]`
	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{running, settled, settled, settled}, "", "")

	prURL := "https://github.com/test/repo/pull/1495"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 4 * time.Hour
	// The shipped defaults, which is what the incident ran with.
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: config.DefaultCIRerunTransient}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		// Bounded: the incident's signature is a monitor that never leaves the
		// polling loop, so exhausting the polls must fail the test rather than
		// reproduce the 4-hour wait.
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 6 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected a terminal approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the settled cancelled check to reach an approval gate")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want exactly the cancelled check", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "Behavior portable serial") {
		t.Fatalf("finding %q must name the cancelled check", findings.Items[0].Description)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user: a cancellation is not a code defect", findings.Items[0].Action)
	}
	for _, l := range logs {
		if l == ciChecksPassedMsg || l == ciNoChecksPassedMsg {
			t.Fatalf("a cancelled check must never report checks passed; logs: %v", logs)
		}
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round for a cancellation, got %d", len(ag.calls))
	}
	if strings.Contains(ghLog(t, logFile), "run rerun") {
		t.Fatalf("the default rerun budget authorizes no rerun, gh log:\n%s", ghLog(t, logFile))
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt != nil {
		t.Fatal("a PR with a cancelled check must not be marked ready to merge")
	}
}

// Readiness is read from the PR's live check rollup on every poll, so it always
// describes the commit the forge currently has for that PR. No recorded head
// SHA gates it: a run whose own record still names the pre-advance commit must
// still recognize the green terminal state the forge published for the head the
// pipeline last pushed.
func TestCIStep_GreenChecksAtAdvancedHeadAreRecognizedWhileRunTracksOlderHead(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	// The branch advances past the commit the run still records, the way a
	// pipeline fix commit does mid-run.
	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): align docs")
	gitCmd(t, dir, "push", "origin", "feature")
	advanced := gitCmd(t, dir, "rev-parse", "HEAD")
	if advanced == headSHA {
		t.Fatal("expected the published head to advance past the recorded head")
	}

	env := fakeCIGHSequence(t, "OPEN", []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
	})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: config.DefaultCIRerunTransient}

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
		t.Fatalf("expected monitoring to continue after green checks, got %v", err)
	}
	sawPassed := false
	for _, l := range logs {
		if l == ciChecksPassedMsg {
			sawPassed = true
		}
	}
	if !sawPassed {
		t.Fatalf("expected the green rollup to be recognized, got: %v", logs)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("green checks on the PR's current head must record CI readiness")
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

	t.Log("CI step log:")
	for _, l := range logs {
		t.Logf("    %s", l)
	}
	t.Log("gh commands the monitor issued:")
	for _, l := range strings.Split(strings.TrimSpace(ghLog(t, logFile)), "\n") {
		t.Logf("    gh %s", l)
	}
	t.Logf("outcome: needs_approval=%v finding=%q", outcome.NeedsApproval, findings.Items[0].Description)
	t.Logf("rerun requests: %d", strings.Count(ghLog(t, logFile), "run rerun"))
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

func TestCIStep_ResolvedRerunDoesNotParkALaterGreenHead(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	cancelled := `[{"name":"Repo invariants","state":"SUCCESS","bucket":"pass"},` +
		`{"name":"Behavior portable serial","state":"CANCELLED","bucket":"cancel","completedAt":"2026-08-02T07:54:14Z","link":"https://github.com/test/repo/actions/runs/30738052151/job/91470340751"}]`
	// The head the pipeline watches then advances (its own fix commit), and the
	// re-run job is path-filtered out of the new head's rollup. Everything the
	// forge does report for that head is green.
	greenWithoutRerunCheck := `[{"name":"Repo invariants","state":"SUCCESS","bucket":"pass"}]`

	env, _ := fakeCIGHLoggedSequence(t, "OPEN", []string{
		cancelled,
		greenWithoutRerunCheck,
		greenWithoutRerunCheck,
		greenWithoutRerunCheck,
		greenWithoutRerunCheck,
	}, "", "")

	prURL := "https://github.com/test/repo/pull/1495"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 4 * time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI = config.CI{RerunTransient: 1}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	advancedHeadSHA := ""
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls == 1 {
				if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "no-mistakes(ci): apply fixes")
				gitCmd(t, dir, "push", "origin", "feature")
				advancedHeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				sctx.Run.HeadSHA = advancedHeadSHA
				if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, advancedHeadSHA); err != nil {
					t.Fatal(err)
				}
			}
			if polls >= 6 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a green head must keep monitoring, got outcome %+v err %v", outcome, err)
	}
	for _, l := range logs {
		if strings.Contains(l, "cancelled") && strings.Contains(l, "after its rerun") {
			t.Fatalf("a rerun the provider answered was re-opened; logs: %v", logs)
		}
		if strings.Contains(l, "auto-fixing") || strings.Contains(l, "manual intervention") {
			t.Fatalf("a green head escalated to the CI fixing path; logs: %v", logs)
		}
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round on a green head, got %d", len(ag.calls))
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatalf("expected CI readiness to survive on a green head; logs: %v", logs)
	}
	if advancedHeadSHA == "" || dbRun.HeadSHA != advancedHeadSHA {
		t.Fatalf("run head = %q, want advanced head %q", dbRun.HeadSHA, advancedHeadSHA)
	}

	t.Log("CI step log:")
	for _, l := range logs {
		t.Logf("    %s", l)
	}
}

func TestCIStep_SameHeadGreenRerunEmitsChecksPassed(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	cancelled := `[{"name":"build","state":"CANCELLED","bucket":"cancel","completedAt":"2026-08-02T07:54:14Z","link":"https://github.com/test/repo/actions/runs/1/job/10"}]`
	runPassed := `[{"name":"build","state":"SUCCESS","bucket":"pass","completedAt":"2026-08-02T08:07:02Z","link":"https://github.com/test/repo/actions/runs/1/job/11"}]`
	otherGreen := `[{"name":"repo invariants","state":"SUCCESS","bucket":"pass"}]`
	env, _ := fakeCIGHLoggedSequence(t, "OPEN", []string{cancelled, runPassed, otherGreen, otherGreen, otherGreen}, "", "")

	prURL := "https://github.com/test/repo/pull/1497"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 4 * time.Hour
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
			if polls >= 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("same-head green rerun must keep monitoring, got %v", err)
	}
	sawPassed := false
	for _, log := range logs {
		if log == ciChecksPassedMsg {
			sawPassed = true
		}
		if strings.Contains(log, "after its rerun") || strings.Contains(log, "manual intervention") {
			t.Fatalf("same-head green rerun was re-opened: %v", logs)
		}
	}
	if !sawPassed {
		t.Fatalf("same-head green rerun never emitted checks-passed: %v", logs)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("same-head green rerun did not set CI readiness")
	}
	if _, ok := step.transientReruns.rollup["build"]; ok {
		t.Fatal("same-head green rerun record was not retired")
	}
}

func TestCIStep_DelayedSameNameCheckRetainsLegacyNameBehavior(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	cancelled := `[{"name":"build","state":"CANCELLED","bucket":"cancel","completedAt":"2026-08-02T07:54:14Z","link":"https://github.com/test/repo/actions/runs/1/job/10"}]`
	delayedSibling := `[{"name":"build","state":"SUCCESS","bucket":"pass","completedAt":"2026-08-02T08:07:02Z","link":"https://github.com/test/repo/actions/runs/2/job/20"}]`
	env, _ := fakeCIGHLoggedSequence(t, "OPEN", []string{cancelled, delayedSibling, delayedSibling, delayedSibling}, "", "")

	prURL := "https://github.com/test/repo/pull/1496"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 4 * time.Hour
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
			if polls >= 6 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("delayed sibling outcome = %+v, err = %v, want continued monitoring", outcome, err)
	}
	sawPassed := false
	for _, log := range logs {
		if log == ciChecksPassedMsg {
			sawPassed = true
		}
	}
	if !sawPassed {
		t.Fatalf("expected legacy name-keyed green behavior, logs: %v", logs)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.CIReadyAt == nil {
		t.Fatal("delayed same-named sibling did not retain legacy readiness behavior")
	}
	if _, ok := step.transientReruns.rollup["build"]; ok {
		t.Fatal("new conclusive link did not retire the rerun record")
	}
}
