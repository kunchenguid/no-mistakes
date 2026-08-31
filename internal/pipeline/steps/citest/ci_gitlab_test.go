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
	env := stepstest.FakeCIGlab(t, "opened", checksJSON)

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
	_, err := step.Execute(sctx)
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

	env := stepstest.FakeCIGlab(t, "merged", "[]")

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
	outcome, err := step.Execute(sctx)
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
	env := stepstest.FakeCIGlab(t, "opened", checksJSON)

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
	outcome, err := step.Execute(sctx)
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
	env := stepstest.FakeCIGlabConflict(t, "opened", checksJSON, true)

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
	outcome, err := step.Execute(sctx)
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
	env := stepstest.FakeCIGlabWithTrace(t, "opened", checksJSON, "stack trace output from gitlab job")

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
	outcome, err := step.Execute(sctx)
	assertCIRestartsValidation(t, outcome, err)
	if capturedPrompt == "" {
		t.Fatal("expected GitLab auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "stack trace output from gitlab job") {
		t.Fatalf("expected GitLab auto-fix prompt to include job trace, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_GitLabPendingChecksKeepMonitoringWhenDone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	sequence := []string{
		`[{"id":1,"name":"build","status":"running"}]`,
		`[{"id":1,"name":"build","status":"success"}]`,
	}
	env := stepstest.FakeCIGlabSequence(t, "opened", sequence)

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
	_, err := step.Execute(sctx)
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
