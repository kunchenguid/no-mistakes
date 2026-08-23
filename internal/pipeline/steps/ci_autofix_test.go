package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	outcome, err := step.Execute(sctx)
	assertCIRestartsValidation(t, outcome, err)
	if !agentCalled {
		t.Error("expected agent to be called for CI auto-fix")
	}

	if len(ag.calls) == 0 {
		t.Fatal("expected agent call")
	}

	foundAutoFix := false
	for _, l := range logs {
		if strings.Contains(l, "issues detected") && strings.Contains(l, "auto-fixing") {
			foundAutoFix = true
			break
		}
	}
	if !foundAutoFix {
		t.Errorf("expected issue detection in logs, got: %v", logs)
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
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed when CI auto-fix is disabled")
	}
	if outcome.AutoFixable {
		t.Fatal("expected manual intervention outcome to be non-auto-fixable")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if findings.Summary != "CI failures require manual intervention" {
		t.Fatalf("findings summary = %q, want %q", findings.Summary, "CI failures require manual intervention")
	}
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 failing-check findings, got %d: %+v", len(findings.Items), findings.Items)
	}
	if findings.Items[0].Description != "CI check failing: lint" {
		t.Fatalf("first finding = %q, want %q", findings.Items[0].Description, "CI check failing: lint")
	}
	if findings.Items[1].Description != "CI check failing: test" {
		t.Fatalf("second finding = %q, want %q", findings.Items[1].Description, "CI check failing: test")
	}

	// Agent should NOT have been called
	if len(ag.calls) > 0 {
		t.Errorf("expected no agent calls when ci=0, got %d", len(ag.calls))
	}

	// Should log that auto-fix is disabled
	foundDisabled := false
	for _, l := range logs {
		if strings.Contains(l, "auto-fix disabled") {
			foundDisabled = true
			break
		}
	}
	if !foundDisabled {
		t.Errorf("expected 'auto-fix disabled' in logs, got: %v", logs)
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
	outcome, err := step.Execute(sctx)
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
	outcome, err = (&CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}).Execute(sctx)
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

	localNow := start.Add(30 * time.Minute)
	step := &CIStep{
		now: func() time.Time { return localNow },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			localNow = localNow.Add(3 * time.Minute)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
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
	outcome, err := step.Execute(sctx)
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

	outcome, err := step.Execute(sctx)
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

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
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
	outcome, err := step.Execute(sctx)
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
			// Agent "investigates" but produces NO changes
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
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed after exhausting fix attempts with no changes")
	}

	if fixCount != 1 {
		t.Fatalf("expected 1 fix attempt (limit=1), got %d", fixCount)
	}

	outcome, err = (&CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}).Execute(sctx)
	if err != nil {
		t.Fatalf("recovered Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("recovered outcome = %#v, want approval after exhausted limit", outcome)
	}
	if fixCount != 1 {
		t.Fatalf("recovered CI made %d total attempts, want 1", fixCount)
	}

	// Should eventually hit max attempts, not spin forever
	foundExhausted := false
	for _, l := range logs {
		if strings.Contains(l, "max auto-fix attempts") {
			foundExhausted = true
			break
		}
	}
	if !foundExhausted {
		t.Errorf("expected 'max auto-fix attempts' in logs, got: %v", logs)
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
	outcome, err := step.Execute(sctx)
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
	step.Execute(sctx)

	if capturedPrompt == "" {
		t.Fatal("expected agent to be called with a prompt")
	}
	if !strings.Contains(capturedPrompt, "You MUST produce file changes") {
		t.Errorf("prompt should instruct agent to produce changes, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "smallest correct root-cause fix") {
		t.Errorf("prompt should prefer root-cause fixes over bandaids, got:\n%s", capturedPrompt)
	}
	assertTestQualityRulePrompt(t, capturedPrompt)
	if strings.Contains(capturedPrompt, "Make the minimal change needed") {
		t.Errorf("prompt should not prefer narrow minimal changes, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "user wanted CI autofix to preserve the extracted intent") {
		t.Errorf("prompt should include extracted user intent, got:\n%s", capturedPrompt)
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

	_, err := (&CIStep{}).autoFixCI(sctx, host, pr, []string{"build"}, false)
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

	if _, err := (&CIStep{}).autoFixCI(sctx, host, pr, []string{"build"}, false); err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("late successful return error = %v, want timeout", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "status", "--porcelain", "--", "ci-fix.txt"); got != "?? ci-fix.txt" {
		t.Fatalf("ci-fix.txt status = %q, want uncommitted", got)
	}
}
