package citest

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
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// pinCIMonitorClock takes a CI-monitor test off the wall clock and off the
// network.
//
// The idle timeout is measured with step.now, and every poll re-arms it by
// resolving the upstream default-branch tip - which, for a repo whose upstream
// is gitlab.com, is a real fetch over the network. Leaving both live makes the
// test assert that that round trip plus several fake-CLI subprocess spawns all
// finish inside CITimeout; a loaded runner does not guarantee that, and when it
// does not, the monitor times out before it has ever read a check and reports
// "PR was still open when CI monitoring timed out" instead of the outcome under
// test. None of these tests exercise timeout re-arming, so both inputs are
// pinned to fixed values.
func pinCIMonitorClock(step *steps.CIStep) {
	frozen := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	step.SetNow(func() time.Time { return frozen })
	step.SetBaseBranchTip(func(context.Context) (string, bool) { return "base-tip-sha", true })
}

// failOnExtraPoll is the waitForNextPoll for a test whose step must resolve on
// its first poll. Under a frozen clock nothing else would stop the loop, so a
// regression has to surface as this error rather than as a hang.
func failOnExtraPoll(context.Context, time.Duration) error {
	return errors.New("CI monitor polled again instead of resolving on its first poll")
}

func TestCIStep_GitLabPassesWhenJobsPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"},{"id":2,"name":"test","status":"success"}]`
	env := stepstest.FakeCIGlab(t, "opened", checksJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected passing GitLab CI to keep monitoring while MR is open, got %v", err)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected passing CI log, got: %v", logs)
	}
}

func TestCIStep_GitLabMergedMRExitsEarly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	env := stepstest.FakeCIGlab(t, "merged", "[]", headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected merged MR to complete without approval")
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "merged") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'merged' in logs, got: %v", logs)
	}
}

func TestCIStep_GitLabFailureNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"},{"id":2,"name":"test","status":"failed"}]`
	env := stepstest.FakeCIGlab(t, "opened", checksJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected GitLab CI failure to require approval when auto-fix is disabled")
	}

	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "test") {
		t.Fatalf("expected failing 'test' check finding, got %+v", findings.Items)
	}
}

func TestCIStep_GitLabMergeConflictDetected(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"}]`
	env := stepstest.FakeCIGlabConflict(t, "opened", checksJSON, headSHA, true)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected merge conflict to require approval")
	}

	var findings types.Findings
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

func TestCIStep_GitLabAutoFixIncludesJobTrace(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	stepstest.GitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	stepstest.GitCmd(t, dir, "config", "user.name", "test")
	stepstest.GitCmd(t, dir, "config", "user.email", "test@test.com")
	stepstest.GitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, dir, "remote", "add", "origin", upstream)
	stepstest.GitCmd(t, dir, "push", "origin", "main")

	stepstest.GitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "feature")
	headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"id":99,"name":"test","status":"failed"}]`
	env := stepstest.FakeCIGlabWithTrace(t, "opened", checksJSON, "stack trace output from gitlab job", headSHA)

	var capturedPrompt string
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "gitlab-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	sctx.Config.CI.RevalidateRepairs = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	assertCIRestartsValidation(t, outcome, err)
	if capturedPrompt == "" {
		t.Fatal("expected GitLab auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "stack trace output from gitlab job") {
		t.Fatalf("expected GitLab auto-fix prompt to include job trace, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_GitLabStalePipelineNeverReportsPassed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// GitLab keeps a merge request's head-pipeline pointer on the last pipeline
	// until one matching a newer source revision exists, so a pipeline that ran
	// at an older commit can still be what a reader sees. Its green jobs say
	// nothing about the commit this run is delivering: the monitor must keep
	// waiting instead of reporting all checks passed.
	stalePipelineSHA := strings.Repeat("f", 40)
	checksJSON := `[{"id":1,"name":"build","status":"success"}]`
	env := stepstest.FakeCIGlabStalePipeline(t, "opened", checksJSON, headSHA, stalePipelineSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the monitor to keep waiting for a pipeline bound to the head, got %v", err)
	}
	foundWait := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			t.Fatalf("an older commit's pipeline was reported as this head's checks: %v", logs)
		}
		if strings.Contains(line, "waiting for the delivered commit's own checks") {
			foundWait = true
		}
	}
	if !foundWait {
		t.Fatalf("expected the stranded pipeline to be reported as a wait, got: %v", logs)
	}
}

// TestCIStep_GitLabHeadBindingRefusalWaitsForTheDeliveredCommit pins the
// distinction between a head-binding refusal and a provider failure. The merge
// request is at the run's head while its head pipeline still ran at an older
// commit - the window a push opens, and the state a stale pointer leaves behind
// when no pipeline is created for the new head. The read is refused, and the
// monitor must keep waiting for the head's own pipeline: a refusal must not
// count toward the consecutive-read-error limit, which would park the run after
// a few minutes telling the operator to check a CLI that answered fine.
func TestCIStep_GitLabHeadBindingRefusalWaitsForTheDeliveredCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"}]`
	env := stepstest.FakeCIGlabStalePipeline(t, "opened", checksJSON, headSHA, strings.Repeat("b", 40))

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls > steps.ConsecutiveCheckErrorLimit()+2 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the monitor to keep waiting for the head's own pipeline, got outcome=%+v err=%v", outcome, err)
	}
	if polls <= steps.ConsecutiveCheckErrorLimit() {
		t.Fatalf("expected more polls than the read-error limit (%d), got %d", steps.ConsecutiveCheckErrorLimit(), polls)
	}
	var sawWait, sawReadFailure, sawPark, sawPassed bool
	for _, line := range logs {
		switch {
		case strings.Contains(line, "waiting for the delivered commit's own checks"):
			sawWait = true
		case strings.Contains(line, "could not check CI"):
			sawReadFailure = true
		case strings.Contains(line, "parking for a decision"):
			sawPark = true
		case strings.Contains(line, "all CI checks passed"):
			sawPassed = true
		}
	}
	if !sawWait {
		t.Fatalf("expected the head-binding refusal to be reported as a wait, got: %v", logs)
	}
	if sawReadFailure || sawPark {
		t.Fatalf("head-binding refusal was treated as a provider read failure: %v", logs)
	}
	if sawPassed {
		t.Fatalf("stranded pipeline was reported as this head's checks: %v", logs)
	}
}

// TestCIStep_GitLabProviderReadFailureStillParks keeps the other half of that
// distinction: a merge request read that genuinely fails is still a read
// failure, so after the consecutive-error limit the step parks with the
// provider-neutral CLI and credentials finding.
func TestCIStep_GitLabProviderReadFailureStillParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	env := stepstest.FakeCIGlabMRReadFails(t, "opened", `[]`, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		return nil
	})
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected a provider read failure to park for a decision, got %+v", outcome)
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "could not be read from the provider") ||
		!strings.Contains(findings.Items[0].Description, "credentials") {
		t.Fatalf("expected the provider read-failure finding, got %+v", findings.Items)
	}
	var sawReadFailure, sawPark bool
	for _, line := range logs {
		if strings.Contains(line, "could not check CI") {
			sawReadFailure = true
		}
		if strings.Contains(line, "parking for a decision") {
			sawPark = true
		}
	}
	if !sawReadFailure || !sawPark {
		t.Fatalf("expected read failures and the park in the step log, got: %v", logs)
	}
}

func TestCIStep_GitLabPendingChecksKeepMonitoringWhenDone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	sequence := []string{
		`[{"id":1,"name":"build","status":"running"}]`,
		`[{"id":1,"name":"build","status":"success"}]`,
	}
	env := stepstest.FakeCIGlabSequence(t, "opened", sequence, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		pollCount++
		if pollCount == 1 {
			return nil
		}
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected passing GitLab CI to keep monitoring while MR is open, got %v", err)
	}
	if pollCount != 2 {
		t.Fatalf("expected one pending wait plus one healthy monitoring wait, got %d", pollCount)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed - still monitoring until merged or closed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected continued-monitoring pass log, got: %v", logs)
	}
}

// TestCIStep_GitLabReviewBotDiscussionCommentsParkUnderAlways is the GitLab
// half of the review-bot comment read: an unresolved discussion note left by a
// registered review bot's GitLab account (greptileai - GitLab has no app slug
// and no "[bot]" login) becomes an ask-user finding under
// ci.review_bot_comments: always, exactly as a GitHub review thread does, and
// the head is not reported checks-passed over it.
//
// GitLab's job objects name no publishing application, so there is no check
// identity to make `on_failure` (the default) fire: the comment read is the
// whole integration here, and it runs on the green/not-yet-registered path.
func TestCIStep_GitLabReviewBotDiscussionCommentsParkUnderAlways(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"}]`
	discussionsJSON := `[{"id":"d1","notes":[{"id":1126,"body":"This retry loop can spin forever","resolvable":true,"resolved":false,"author":{"username":"greptileai"},"position":{"new_path":"internal/app.go","new_line":42}}]}]`
	env := stepstest.FakeCIGlabWithReviewComments(t, "opened", checksJSON, discussionsJSON, headSHA)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.CI.ReviewBotComments = config.CIReviewBotCommentsAlways

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want a blocking, non-auto-fixable park", outcome)
	}
	findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one per unresolved bot discussion note", findings.Items)
	}
	item := findings.Items[0]
	if item.Action != types.ActionAskUser || item.Severity != types.FindingSeverityWarning || item.Category != types.FindingCategoryCIReviewBot {
		t.Fatalf("finding = %+v, want an ask-user ci-review-bot warning", item)
	}
	if item.File != "internal/app.go" || item.Line != 42 || !strings.Contains(item.Description, "greptileai") || !strings.Contains(item.Description, "spin forever") {
		t.Fatalf("finding = %+v, want the GitLab bot login, the note's file and line, and its body", item)
	}
	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "all CI checks passed") {
		t.Fatalf("logs = %v, checks-passed must not be reported over an unresolved bot comment", logs)
	}
}

// Under the always policy, only a successfully read empty discussion array
// permits readiness. Invalid provider output must park without checks-passed.
func TestCIStep_GitLabDiscussionReadRequiresValidArray(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"empty array", "[]", true},
		{"blank output", " \r\n", false},
		{"null", "null", false},
		{"non JSON", "request failed", false},
		{"object", "{}", false},
		{"invalid later page", "[]\nnull", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			env := stepstest.FakeCIGlabWithReviewComments(t, "opened",
				`[{"id":1,"name":"build","status":"success"}]`, tc.payload, headSHA)
			prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
			sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
			sctx.Run.PRURL = &prURL
			sctx.Config.CI.ReviewBotComments = config.CIReviewBotCommentsAlways
			var logs []string
			sctx.Log = func(s string) { logs = append(logs, s) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx
			polls := 0
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
				polls++
				if tc.valid || polls > 10 {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			pinCIMonitorClock(step)
			outcome, err := step.Execute(sctx)
			passed := strings.Contains(strings.Join(logs, "\n"), "all CI checks passed")
			if tc.valid {
				if !errors.Is(err, context.Canceled) || !passed {
					t.Fatalf("valid empty array: error = %v, logs = %v", err, logs)
				}
				return
			}
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || passed {
				t.Fatalf("unreadable discussions: outcome = %#v, error = %v, logs = %v", outcome, err, logs)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("findings = %+v, error = %v", findings, err)
			}
			if item := findings.Items[0]; item.Action != types.ActionAskUser || !strings.Contains(item.Description, "could not be read") {
				t.Fatalf("finding = %+v, want ask-user read failure", item)
			}
		})
	}
}

// gitlabCIFindings unmarshals the findings a GitLab CI outcome carries.
func gitlabCIFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	return findings
}

func TestCIStep_GitLabFailedPipelineWithNoJobsEscalates(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// The reported case: GitLab reports the head pipeline failed with an empty
	// job list (the configuration was rejected, so no job ever existed). Read
	// through the job list alone it looked like "no checks registered", and the
	// monitor waited for checks that would never arrive instead of escalating
	// the failure it was looking at.
	env := stepstest.FakeCIGlab(t, "opened", "[]", headSHA)
	env = append(env, "FAKE_CLI_PIPELINE_STATUS=failed")

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a failed head pipeline with no jobs to require approval")
	}
	findings := gitlabCIFindings(t, outcome)
	found := false
	for _, finding := range findings.Items {
		if finding.Check == "Pipeline #7" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a finding for the failed pipeline, got %+v", findings.Items)
	}
}

func TestCIStep_GitLabFailedTriggerJobEscalates(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// A failed trigger job leaves every ordinary job green, so the job list
	// alone reads as an all-green head even though the pipeline failed.
	env := stepstest.FakeCIGlab(t, "opened", `[{"id":1,"name":"build","status":"success"}]`, headSHA)
	env = append(env,
		"FAKE_CLI_PIPELINE_STATUS=failed",
		`FAKE_CLI_TRIGGER_JOBS=[{"id":5,"name":"deploy-child","status":"failed","downstream_pipeline":{"id":9,"status":"failed"}}]`,
	)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a failed trigger job to require approval")
	}
	findings := gitlabCIFindings(t, outcome)
	found := false
	for _, finding := range findings.Items {
		if finding.Check == "deploy-child" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a finding for the failed trigger job, got %+v", findings.Items)
	}
}

func TestCIStep_GitLabPendingPipelineIsNotReady(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// Every ordinary job has finished green while the pipeline itself is still
	// running. Readiness must keep waiting, not report checks-passed.
	env := stepstest.FakeCIGlab(t, "opened", `[{"id":1,"name":"build","status":"success"}]`, headSHA)
	env = append(env, "FAKE_CLI_PIPELINE_STATUS=running")

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a pending pipeline to keep monitoring while the MR is open, got %v", err)
	}
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			t.Fatalf("a still-running pipeline was reported as all checks passed: %v", logs)
		}
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "CI checks running") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the monitor to report the pipeline as running, got: %v", logs)
	}
}

func TestCIStep_GitLabFailedExternalStatusCheckEscalates(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// An external status check is a merge requirement like any other; a failing
	// one must not read as an all-green head just because the jobs are green.
	env := stepstest.FakeCIGlab(t, "opened", `[{"id":1,"name":"build","status":"success"}]`, headSHA)
	env = append(env,
		`FAKE_CLI_STATUS_CHECKS=[{"id":3,"name":"Compliance","external_url":"https://compliance.example.com/mr/42","status":"failed"}]`,
	)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a failed external status check to require approval")
	}
	findings := gitlabCIFindings(t, outcome)
	found := false
	for _, finding := range findings.Items {
		if finding.Check == "Compliance" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a finding for the failed external status check, got %+v", findings.Items)
	}
}

func TestCIStep_GitLabExternalStatusCheckRouteAbsentStillPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	// A Community Edition instance does not mount the external status check
	// route at all (an Ultimate capability); it must read as having none, not
	// as an unreadable check that parks the run, and not as a pending one that
	// never resolves.
	env := stepstest.FakeCIGlab(t, "opened", `[{"id":1,"name":"build","status":"success"}]`, headSHA)
	env = append(env, "FAKE_CLI_GLAB_STATUS_CHECKS_ROUTE=absent")

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the green head to keep monitoring while the MR is open, got %v", err)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected checks-passed on an instance without external status checks, got: %v", logs)
	}
}
