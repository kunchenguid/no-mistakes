package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func assertCIRestartsValidation(t *testing.T, outcome *pipeline.StepOutcome, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CI repair returned error: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("CI repair outcome = %#v, want restart from review", outcome)
	}
}

func TestCIStep_CIFailureAutoFix(t *testing.T) {
	t.Parallel()
	// Set up upstream bare repo for push
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

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	agentCalled := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			// Agent "fixes" CI by creating a file
			os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.UserIntent = "user wanted CI autofix to preserve the extracted intent"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI.RevalidateRepairs = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 2 {
				cancel()
			}
			return ctx.Err()
		},
	}
	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if !agentCalled {
		t.Error("expected agent to be called for CI auto-fix")
	}

	if len(ag.calls) == 0 {
		t.Fatal("expected agent call")
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "issues detected: 1 CI check failing") || !strings.Contains(joined, "repairing: test") {
		t.Errorf("expected the observation and the fix round in logs, got: %v", logs)
	}
}

func TestCIStep_CIAutoFixDisabledWithZero(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[
		{"name":"build","state":"SUCCESS","bucket":"pass"},
		{"name":"test","state":"FAILURE","bucket":"fail"},
		{"name":"lint","state":"ACTION_REQUIRED","bucket":"fail"},
		{"name":"deploy","state":"NEUTRAL"}
	]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	ag := &mockAgent{name: "test"}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled
	sctx.Config.CITimeout = 3 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	// The observation is the same whatever auto_fix.ci says: one auto-fix
	// finding per failing check, blocking. Enforcing the zero limit is the
	// executor's job (TestExecutor_AutoFixDisabledWithZero), which is why the
	// driver above never re-executed the step.
	if !outcome.NeedsApproval {
		t.Fatal("expected a blocking observation when CI checks fail")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected the failing-check observation to be auto-fixable for the executor to gate")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if findings.Summary != "2 CI checks failing" {
		t.Fatalf("findings summary = %q, want %q", findings.Summary, "2 CI checks failing")
	}
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 failing-check findings, got %d: %+v", len(findings.Items), findings.Items)
	}
	for i, want := range []string{"lint", "test"} {
		item := findings.Items[i]
		if !strings.HasPrefix(item.Description, "CI check failing: "+want) {
			t.Fatalf("finding %d = %q, want it to name %q", i, item.Description, want)
		}
		if item.Check != want || item.Category != types.FindingCategoryCICheck || item.Action != types.ActionAutoFix || item.Severity != types.FindingSeverityError {
			t.Fatalf("finding %d = %+v, want an auto-fix ci-check error for %q", i, item, want)
		}
	}

	// Agent should NOT have been called
	if len(ag.calls) > 0 {
		t.Errorf("expected no agent calls when ci=0, got %d", len(ag.calls))
	}
	if pollCount != 0 {
		t.Errorf("expected the settled observation to return before polling again, got %d polls", pollCount)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "issues detected: 2 CI checks failing") {
		t.Errorf("expected the observation to be logged, got: %v", logs)
	}
}

func TestCIStep_CIAutoFixLimitExhausted(t *testing.T) {
	t.Parallel()
	// Set up upstream bare repo for push
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
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			// Agent "fixes" but the check will keep failing (same checksJSON)
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1} // only 1 attempt allowed
	sctx.Config.CI.RevalidateRepairs = true
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Errorf("expected 1 auto-fix attempt (limit=1), got %d", fixCount)
	}
	if _, err := sctx.DB.InsertStepRound(stepResult.ID, 1, "auto_fix", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	// A recovered run restores its spent attempt count from the round history
	// and re-executes the step as a fresh observation; with the limit already
	// spent, the executor parks instead of starting another round.
	sctx.Fixing = false
	sctx.PreviousFindings = ""
	outcome, err = stepstest.ExecuteWithAutoFix(t, &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}, sctx, 1)
	if err != nil {
		t.Fatalf("recovered Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("recovered outcome = %#v, want approval after exhausted limit", outcome)
	}
	if fixCount != 1 {
		t.Fatalf("recovered CI made %d total repairs, want 1", fixCount)
	}
}

func TestCIStep_CIAutoFixRetriesAfterChecksRerun(t *testing.T) {
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

	checksSequence := []string{
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
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
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected one local repair before revalidation, got %d", fixCount)
	}
}

func TestCIStep_CIAutoFixRetriesWhenGitHubClockLagsLocalClock(t *testing.T) {
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

	start := time.Date(2026, 4, 24, 4, 14, 0, 0, time.UTC)
	oldCompletedAt := start.Add(1 * time.Minute).Format(time.RFC3339)
	newCompletedAt := start.Add(2 * time.Minute).Format(time.RFC3339)
	checksSequence := []string{
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, oldCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 5 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	localNow := start.Add(30 * time.Minute)
	step := &CIStep{
		now: func() time.Time { return localNow },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			localNow = localNow.Add(3 * time.Minute)
			return nil
		},
	}

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected one local repair before revalidation, got %d", fixCount)
	}
}

// TestCIStep_CIAutoFixRetriesWhenFastChecksSkipPendingObservation reproduces
// the real-world scenario where a failing CI check completes so fast between
// polls that the pipeline never observes it in a pending state, but the check's
// completedAt timestamp moves past the last-fix time - proving CI re-ran. The
// pipeline should treat the second failure as a new iteration and attempt
// another fix rather than logging "fix already attempted" indefinitely.
func TestCIStep_CIAutoFixRetriesWhenFastChecksSkipPendingObservation(t *testing.T) {
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

	// Simulate a fake "now" that advances across polls. The failing check's
	// completedAt on poll 2 is after the autofix push time, proving CI re-ran.
	// But neither poll observes a pending state - the pipeline must detect
	// the rerun from completedAt.
	start := time.Date(2026, 4, 24, 4, 14, 0, 0, time.UTC)
	oldCompletedAt := start.Add(1 * time.Minute).Format(time.RFC3339)  // pre-fix failure
	newCompletedAt := start.Add(10 * time.Minute).Format(time.RFC3339) // post-fix failure (rerun)
	checksSequence := []string{
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, oldCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 1 * time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	fakeNow := start
	step := &CIStep{
		now: func() time.Time { return fakeNow },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			// Advance fake clock past the autofix push so the second poll's
			// check completedAt looks "after" lastFixedAt.
			fakeNow = fakeNow.Add(3 * time.Minute)
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected one local repair before revalidation, got %d", fixCount)
	}
}

// TestCIStep_CIAutoFixRetriesWhenSomeChecksStayFailing reproduces the real-world
// scenario where multiple checks fail, the fix push causes only some of them to
// re-run (and thus transit through pending) while at least one check keeps
// reporting as failing throughout. The pipeline should still recognize the
// post-rerun same-name failure as a new attempt and progress to attempt 2,
// rather than logging "fix already attempted" indefinitely until CI timeout.
func TestCIStep_CIAutoFixRetriesWhenSomeChecksStayFailing(t *testing.T) {
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

	// At least one check stays failing throughout the push+rerun transition,
	// so `failing` is never empty and the original "all pass" reset never fires.
	checksSequence := []string{
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"IN_PROGRESS","bucket":"pending"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"IN_PROGRESS","bucket":"pending"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
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
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected one local repair before revalidation, got %d", fixCount)
	}
}

func TestCIStep_DoesNotRetryOnUnrelatedPendingCheck(t *testing.T) {
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

	checksSequence := []string{
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"docs","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
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
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 3 {
				cancel()
			}
			return ctx.Err()
		},
	}

	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected unrelated pending checks not to trigger a second auto-fix attempt, got %d", fixCount)
	}

}

func TestCIStep_RetriesMergeConflictAfterRerun(t *testing.T) {
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

	checksSequence := []string{
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
		`[{"name":"build","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
		`[{"name":"build","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
	}
	env := fakeCIGHSequenceMergeable(t, "OPEN", checksSequence, "CONFLICTING")

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("conflict-fix-%d.txt", fixCount)), []byte("resolved"), 0o644)
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
	sctx.Config.AutoFix = config.AutoFix{CI: 2}
	sctx.Config.CI.RevalidateRepairs = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected one local repair before revalidation, got %d", fixCount)
	}
}

func TestCIStep_FixMode_ManualInterventionRunsCIFix(t *testing.T) {
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
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, "manual-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix failing CI"}`)}, nil
		},
	}

	findingsJSON, err := json.Marshal(Findings{
		Summary: "CI failures require manual intervention",
		Items: []Finding{{
			ID:          "review-1",
			Severity:    "warning",
			Description: "CI check failing: test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Fixing = true
	sctx.PreviousFindings = string(findingsJSON)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 2 {
				cancel()
			}
			return ctx.Err()
		},
	}
	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if fixCount != 1 {
		t.Fatalf("expected 1 manual CI fix attempt, got %d", fixCount)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
}

// TestCIStep_AutoFixNoChanges_CountsAsAttempt verifies that when the agent
// produces no changes (nothing to commit), it still counts as a consumed fix
// attempt rather than spinning forever with "fix already attempted".
func TestCIStep_AutoFixNoChanges_CountsAsAttempt(t *testing.T) {
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
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			return &agent.Result{Output: json.RawMessage(`{"summary":"test failure still requires a code repair","code_change_needed":true}`)}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed after exhausting fix attempts with no changes")
	}

	if fixCount != 1 {
		t.Fatalf("expected 1 fix attempt (limit=1), got %d", fixCount)
	}
	// A round that produced no change re-emits the same auto-fix findings so
	// the executor can retry while attempts remain; here the limit is spent,
	// so that observation is what parks.
	if !outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want the failing check re-observed as auto-fixable for the executor to park", outcome)
	}

	sctx.Fixing = false
	sctx.PreviousFindings = ""
	outcome, err = stepstest.ExecuteWithAutoFix(t, &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}, sctx, 1)
	if err != nil {
		t.Fatalf("recovered Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("recovered outcome = %#v, want approval after exhausted limit", outcome)
	}
	if fixCount != 1 {
		t.Fatalf("recovered CI made %d total attempts, want 1", fixCount)
	}

	// Should never log "fix already attempted" indefinitely
	waitCount := 0
	for _, l := range logs {
		if strings.Contains(l, "fix already attempted") {
			waitCount++
		}
	}
	if waitCount > 0 {
		t.Errorf("expected no 'fix already attempted' loops when agent produces no changes, got %d", waitCount)
	}
}

func TestCIStep_AutoFixExternalFailureStopsWithAgentConclusion(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	checksJSON := `[{"name":"PR must be raised via no-mistakes","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			fixCount++
			return &agent.Result{Output: json.RawMessage(`{"summary":"attestation failure is external to the PR code","code_change_needed":false}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeCIGH(t, "OPEN", checksJSON)
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	outcome, err := driveCI(t, &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want stopped approval outcome", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want the no-change conclusion parked as ask-user, never re-entering the auto-fix loop", outcome)
	}
	if fixCount != 1 {
		t.Fatalf("fix attempts = %d, want one trusted no-change conclusion", fixCount)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if findings.Summary != "attestation failure is external to the PR code" {
		t.Fatalf("reported conclusion = %q", findings.Summary)
	}
}

// TestCIStep_FixMode_NoChanges_CountsAsAttempt verifies the same no-changes
// behavior for manual fix mode (sctx.Fixing = true).
func TestCIStep_FixMode_NoChanges_CountsAsAttempt(t *testing.T) {
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
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			// Agent produces NO changes
			return &agent.Result{}, nil
		},
	}

	findingsJSON, err := json.Marshal(Findings{
		Summary: "CI failures require manual intervention",
		Items: []Finding{{
			Severity:    "warning",
			Description: "CI check failing: test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Fixing = true
	sctx.PreviousFindings = string(findingsJSON)

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed after fix mode with no changes")
	}

	if fixCount != 1 {
		t.Fatalf("expected 1 manual fix attempt, got %d", fixCount)
	}

	// Should return failure outcome, not spin forever
	foundFailed := false
	for _, l := range logs {
		if strings.Contains(l, "CI fix produced no changes") {
			foundFailed = true
			break
		}
	}
	if !foundFailed {
		t.Errorf("expected 'CI fix produced no changes' in logs, got: %v", logs)
	}
}

// TestCIStep_AutoFixPromptIncludesMustFixInstruction verifies the agent prompt
// includes a strong instruction that the agent must produce changes.
func TestCIStep_AutoFixPromptIncludesMustFixInstruction(t *testing.T) {
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
	env := fakeCIGH(t, "OPEN", checksJSON)

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
	sctx.UserIntent = "user wanted CI autofix to preserve the extracted intent"
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
		t.Fatal("expected agent to be called with a prompt")
	}
	if !strings.Contains(capturedPrompt, "If a failing check is caused by this PR's code") {
		t.Errorf("prompt should still require a code/test failure to be fixed, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "you MUST produce file changes that fix it") {
		t.Errorf("prompt should instruct agent to produce changes for a genuine code defect, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "A real failing test or build must still be fixed") {
		t.Errorf("prompt should keep the genuine-failure mandate, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "you MAY conclude that no code change is warranted") {
		t.Errorf("prompt should allow no-edit when the failing check is not a code defect, got:\n%s", capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "Do not conclude that nothing needs to change") {
		t.Errorf("prompt should not force an edit for every red check, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "smallest correct root-cause fix") {
		t.Errorf("prompt should prefer root-cause fixes over bandaids, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Fix the reported instance narrowly") {
		t.Errorf("prompt should scope the fix to the reported instance, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Prefer doing so by addressing a deeper architectural reason and simplifying it, than introducing machinery to handle the symptoms") {
		t.Errorf("prompt should prefer simplification over symptom machinery, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Do not add new subsystems, guards, instructions, or behaviors beyond what the specific failing check requires") {
		t.Errorf("prompt should forbid extra machinery, got:\n%s", capturedPrompt)
	}
	assertTestQualityRulePrompt(t, capturedPrompt)
	if strings.Contains(capturedPrompt, "Make the minimal change needed") {
		t.Errorf("prompt should not prefer narrow minimal changes, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "user wanted CI autofix to preserve the extracted intent") {
		t.Errorf("prompt should include extracted user intent, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, dir) || !strings.Contains(capturedPrompt, "Path contract:") {
		t.Errorf("prompt should include execution context with workdir, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_FixPromptPrefersSimplificationOverMachinery(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if _, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Fix the reported instance narrowly.",
		"Prefer doing so by addressing a deeper architectural reason and simplifying it, than introducing machinery to handle the symptoms.",
		"Do not add new subsystems, guards, instructions, or behaviors beyond what the specific failing check requires",
		"smallest correct root-cause fix",
	} {
		if !strings.Contains(capturedPrompt, want) {
			t.Errorf("CI fix prompt missing narrow-fix contract %q:\n%s", want, capturedPrompt)
		}
	}
	if strings.Contains(capturedPrompt, "fix the deepest practical cause instead") {
		t.Errorf("CI fix prompt still licenses expanding to the deepest practical cause:\n%s", capturedPrompt)
	}
}

func TestCIStep_FixPromptDistinguishesCodeDefectFromExternalFailure(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if _, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"PR must be raised via no-mistakes"}, false)); err != nil {
		t.Fatal(err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected the CI fixer prompt to be constructed")
	}
	if !strings.Contains(capturedPrompt, "A real failing test or build must still be fixed") {
		t.Errorf("prompt lost the genuine-failure mandate:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, `you MUST produce file changes that fix it`) {
		t.Errorf("prompt lost the code-defect must-fix rule:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "you MAY conclude that no code change is warranted") {
		t.Errorf("prompt should allow no-edit for a non-code check failure:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "not caused by the code under review") {
		t.Errorf("prompt should draw the caused-by-this-PR-code line:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "PR must be raised via no-mistakes") {
		t.Errorf("prompt should name the attestation check as a non-code example:\n%s", capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "Do not conclude that nothing needs to change") {
		t.Errorf("prompt should not force an edit for every red check:\n%s", capturedPrompt)
	}
}

func TestCIStep_HangingFixAgentFailsAfterTimeout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "hanging-ci-fix-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond
	host := &forgejoLogTestHost{}
	pr := &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}

	_, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor([]string{"build"}, false))
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("hanging CI fix error = %v, want timeout", err)
	}
}

func TestCIStep_FixAgentSuccessfulReturnAfterTimeoutFailsWithoutCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "late-ci-fix-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond
	host := &forgejoLogTestHost{}
	pr := &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}

	if _, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor([]string{"build"}, false)); err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("late successful return error = %v, want timeout", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "status", "--porcelain", "--", "ci-fix.txt"); got != "?? ci-fix.txt" {
		t.Fatalf("ci-fix.txt status = %q, want uncommitted", got)
	}
}

type mockReviewHost struct {
	scm.Host
	calls    int
	comments []scm.ReviewComment
}

func (m *mockReviewHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{ReviewComments: true}
}

func (m *mockReviewHost) GetReviewComments(context.Context, *scm.PR) ([]scm.ReviewComment, error) {
	m.calls++
	return m.comments, nil
}

func TestCIStep_AutoFixUsesOnlySelectedFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			return &agent.Result{}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	host := &mockReviewHost{comments: []scm.ReviewComment{{Author: "greptile-apps[bot]", Body: "unselected bot finding"}}}
	pr := &scm.PR{Number: "869", URL: "https://github.com/kunchenguid/no-mistakes/pull/869"}

	_, _ = (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor([]string{"test"}, false))

	if host.calls != 0 || strings.Contains(capturedPrompt, "unselected bot finding") {
		t.Fatalf("unselected review comments reached the fixer: calls=%d prompt=%q", host.calls, capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, `"check":"test"`) {
		t.Fatalf("selected finding missing from prompt:\n%s", capturedPrompt)
	}
}

// TestCIStep_FixAgentBudgetExhaustionParksForADecisionInsteadOfRetrying pins the
// bounded outcome for a CI auto-fix agent that burns its whole invocation
// budget without finishing.
//
// The failure this replaces: the timeout was downgraded to a step-log warning
// and the poll loop re-issued the identical request on the next tick, up to
// auto_fix.ci attempts. Each retry cost another full agent budget, produced no
// operator-visible signal outside the CI step log, and ended the run at
// ci_timeout hours later with nothing to act on.
func TestCIStep_FixAgentBudgetExhaustionParksForADecisionInsteadOfRetrying(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"greptile","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	var invocations int
	ag := &mockAgent{
		name: "wedged",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			invocations++
			<-ctx.Done()
			return nil, errors.New("pi exited: unable to access 'https://operator:secret@example.com/owner/repo.git': denied")
		},
	}

	prURL := "https://github.com/test/repo/pull/3195"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 10}
	sctx.Config.AgentTimeout = 50 * time.Millisecond

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	polls := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls > 3 {
				t.Fatal("CI monitor kept polling after the fix agent exhausted its budget")
			}
			return nil
		},
	}

	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("CI step returned error %v, want a parked decision that keeps the run alive", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the step parked for a decision", outcome)
	}
	if invocations != 1 {
		t.Fatalf("agent invocations = %d, want exactly one budget spent before asking", invocations)
	}

	var findings Findings
	if jsonErr := json.Unmarshal([]byte(outcome.Findings), &findings); jsonErr != nil {
		t.Fatalf("parse findings %q: %v", outcome.Findings, jsonErr)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %#v, want one gate finding", findings.Items)
	}
	item := findings.Items[0]
	if item.Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q so the gate parks for a human decision", item.Action, types.ActionAskUser)
	}
	if !strings.Contains(item.Description, "greptile") {
		t.Fatalf("finding %q, want the check it was repairing named", item.Description)
	}
	if !strings.Contains(item.Description, "produced no output at all") {
		t.Fatalf("finding %q, want the measured silence carried into the gate", item.Description)
	}
	if strings.Contains(item.Description, "operator:secret") {
		t.Fatalf("finding %q leaked adapter URL credentials", item.Description)
	}
	if !strings.Contains(item.Description, "https://redacted@example.com/owner/repo.git") {
		t.Fatalf("finding %q, want the adapter URL preserved with credentials redacted", item.Description)
	}
}

// TestCIStep_NonTimeoutFixFailureKeepsRetrying is the counter-test: only a
// proven full-budget burn parks. An ordinary transient fix failure keeps its
// existing warn-and-retry behaviour, because repeating it is cheap and often
// works.
func TestCIStep_NonTimeoutFixFailureKeepsRetrying(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"greptile","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	var invocations int
	ag := &mockAgent{
		name: "flaky",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			invocations++
			return nil, errors.New("transient provider hiccup")
		},
	}

	prURL := "https://github.com/test/repo/pull/3195"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			t.Fatal("a transient fix failure must resume monitoring and re-observe the settled checks, not wait")
			return nil
		},
	}

	// Every failed round resumes monitoring, re-observes the same failing
	// check, and hands the executor another auto-fix observation; the executor
	// (mirrored by the driver) spends the whole auto_fix.ci budget retrying
	// before the last observation parks.
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("CI step returned error %v", err)
	}
	if outcome == nil || !outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want the failing check re-observed as auto-fixable rather than an ask-user park", outcome)
	}
	if invocations != 3 {
		t.Fatalf("agent invocations = %d, want one per auto_fix.ci round", invocations)
	}
	warned := false
	for _, l := range logs {
		if strings.Contains(l, "CI fix failed") {
			warned = true
		}
		if strings.Contains(l, "exceeded its invocation budget") {
			t.Fatalf("logs = %v, a transient failure must not be reported as a budget burn", logs)
		}
	}
	if !warned {
		t.Fatalf("logs = %v, want the transient failure still warned about", logs)
	}
}

// The CI fixer shares the review fixer's removal rule so both apply one
// discipline: a red check caused by a code path the intent does not strictly
// require is fixed by removing that path, not by hardening it.
func TestCIStep_FixPromptPrefersRemovalOfUnrequiredPaths(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			return &agent.Result{}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if _, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"When a problem can be solved by removing a code path that is not strictly required to satisfy the intent",
		"fix it by removing that path, not by validating, hardening, or documenting it",
		"Judge what the intent strictly requires against the User intent section when present, otherwise against the change's own stated purpose",
		"Fix the reported instance narrowly.",
		"Do not add new subsystems, guards, instructions, or behaviors beyond what the specific failing check requires",
	} {
		if !strings.Contains(capturedPrompt, want) {
			t.Errorf("CI fix prompt missing removal-rule contract %q:\n%s", want, capturedPrompt)
		}
	}
}
