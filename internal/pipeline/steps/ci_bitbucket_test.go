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
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_BitbucketPassesWhenStatusesPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withStatuses(`[{"name":"build","state":"SUCCESSFUL"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

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
	_, err := driveCI(t, step, sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Bitbucket CI pass to keep monitoring while PR is open, got %v", err)
	}
	if api.logCount("pull-requests get") == 0 {
		t.Fatal("expected Bitbucket PR state endpoint to be called")
	}
	if api.logCount("--statuses") == 0 {
		t.Fatal("expected Bitbucket statuses endpoint to be called")
	}
	foundPassed := false
	for _, line := range logs {
		if strings.Contains(line, "ready to merge") {
			t.Fatalf("expected Bitbucket CI logs not to imply mergeability, got %v", logs)
		}
		if strings.Contains(line, "all CI checks passed - still monitoring until merged or closed") {
			foundPassed = true
		}
	}
	if !foundPassed {
		t.Fatalf("expected successful Bitbucket CI logs, got %v", logs)
	}
}

func TestCIStep_BitbucketFailureNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withStatuses(`[{"name":"build","key":"build-linux","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/1"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := &CIStep{}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected Bitbucket CI failures to require approval when auto-fix is disabled")
	}
	if api.logCount("pull-requests get") == 0 || api.logCount("--statuses") == 0 {
		t.Fatalf("expected Bitbucket CI endpoints to be called")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("expected failing Bitbucket check finding, got %+v", findings.Items)
	}
	if findings.Items[0].CheckID != "bitbucket-status:build-linux" {
		t.Fatalf("Bitbucket finding CheckID = %q, want exact status identity", findings.Items[0].CheckID)
	}
}

// A stopped Bitbucket pipeline is terminal in the same way a cancelled GitHub
// check is: Bitbucket will not move it to SUCCESSFUL or FAILED on its own. It
// is not a job failure either, so it parks for a decision rather than entering
// the fix loop - and never by continuing to poll a result that has stopped
// changing.
func TestCIStep_BitbucketStoppedCheckParksForADecision(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withStatuses(`[{"name":"build","state":"STOPPED"}]`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

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
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a stopped Bitbucket pipeline to park for a decision")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("findings = %+v, want the stopped check named", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", findings.Items[0].Action)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no fix-agent round for a stopped pipeline, got %d", len(ag.calls))
	}
}

func TestCIStep_BitbucketAutoFixIncludesPipelineLogs(t *testing.T) {
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

	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withSourceCommit(headSHA).
		withStatuses(`[{"name":"test","key":"test","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/1"}]`).
		withPipelineLog("1", "error log output")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	sctx.Config.CI.RevalidateRepairs = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "error log output") {
		t.Fatalf("expected Bitbucket auto-fix prompt to include pipeline logs, got:\n%s", capturedPrompt)
	}
	if api.logCount("pipeline get --pipeline 1") == 0 {
		t.Fatalf("expected the failed pipeline's logs to be fetched")
	}
}

func TestCIStep_BitbucketAutoFixAggregatesSelectedPipelineLogs(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withPRState("OPEN").
		withSourceCommit(headSHA).
		withStatuses(`[{"name":"build","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/1"},{"name":"test","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/2"}]`).
		withPipelineLog("1", "build pipeline log").
		withPipelineLog("2", "test pipeline log")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			if err := os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = api.Env()
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	sctx.Config.CI.RevalidateRepairs = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	outcome, err := driveCI(t, step, sctx)
	assertCIRestartsValidation(t, outcome, err)
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "build pipeline log") || !strings.Contains(capturedPrompt, "test pipeline log") {
		t.Fatalf("expected prompt to include both selected pipeline logs, got:\n%s", capturedPrompt)
	}
	if api.logCount("pipeline get --pipeline 1") == 0 || api.logCount("pipeline get --pipeline 2") == 0 {
		t.Fatalf("expected both Bitbucket pipeline logs to be fetched")
	}
}

func TestResolveBitbucketRepoRef_RedactsCredentialInError(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret_DO_NOT_LEAK"
	// A non-Bitbucket host with embedded credentials: ParseRepoRef rejects the
	// host, no PR URL is set, so the function returns its fallback error. That
	// error surfaces the upstream URL and must not leak the embedded credential.
	credURL := "https://x-access-token:" + token + "@example.com/w/r.git"
	_, err := resolveBitbucketRepoRef(credURL, nil)
	if err == nil {
		t.Fatal("expected error for unresolved non-Bitbucket upstream")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("resolveBitbucketRepoRef error leaked credential: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redacted@example.com/w/r.git") {
		t.Errorf("error missing redacted URL, got: %q", err.Error())
	}
}

func TestLatestBitbucketStatusesKeepsNewestStatusPerCheck(t *testing.T) {
	t.Parallel()

	statuses := []bitbucket.CommitStatus{
		{Name: "build", State: "SUCCESSFUL", Key: "build"},
		{Name: "tests", State: "FAILED", Key: "tests"},
		{Name: "build", State: "FAILED", Key: "build"},
		{Name: "tests", State: "SUCCESSFUL", Key: "tests"},
		{Name: "lint", State: "INPROGRESS"},
	}

	got := bitbucket.LatestStatuses(statuses)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0].Name != "build" || got[0].State != "SUCCESSFUL" {
		t.Fatalf("got[0] = %#v, want latest successful build", got[0])
	}
	if got[1].Name != "tests" || got[1].State != "FAILED" {
		t.Fatalf("got[1] = %#v, want latest failed tests", got[1])
	}
	if got[2].Name != "lint" || got[2].State != "INPROGRESS" {
		t.Fatalf("got[2] = %#v, want pending lint", got[2])
	}
}

func TestLatestBitbucketStatusesDeduplicatesByKeyBeforeName(t *testing.T) {
	t.Parallel()

	statuses := []bitbucket.CommitStatus{
		{Name: "build v2", Key: "build", State: "SUCCESSFUL"},
		{Name: "build", Key: "build", State: "FAILED"},
		{Name: "tests", State: "SUCCESSFUL"},
		{Name: "tests", State: "FAILED"},
	}

	got := bitbucket.LatestStatuses(statuses)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Key != "build" || got[0].Name != "build v2" || got[0].State != "SUCCESSFUL" {
		t.Fatalf("got[0] = %#v, want newest keyed build status", got[0])
	}
	if got[1].Key != "" || got[1].Name != "tests" || got[1].State != "SUCCESSFUL" {
		t.Fatalf("got[1] = %#v, want newest unnamed tests status", got[1])
	}
}

func TestCIStep_GetCIChecksBitbucketFallsBackToKeyWhenNameMissing(t *testing.T) {
	t.Parallel()

	api := newFakeBitbucketAPI(t, 42, "https://bitbucket.org/test/repo/pull-requests/42").
		withStatuses(`[{"key":"build","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/42"}]`)

	host := bitbucket.New(fakeBitbucketCmdFactory(t, api), func() bool { return true }, bitbucket.RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetChecks returned error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Name != "build" {
		t.Fatalf("checks[0].Name = %q, want build", checks[0].Name)
	}
	if checks[0].Bucket != "fail" {
		t.Fatalf("checks[0].Bucket = %q, want fail", checks[0].Bucket)
	}
	if checks[0].ExecutionID != "42" {
		t.Fatalf("checks[0].ExecutionID = %q, want pipeline build number", checks[0].ExecutionID)
	}
}
