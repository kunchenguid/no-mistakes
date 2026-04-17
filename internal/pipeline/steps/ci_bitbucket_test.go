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
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_BitbucketPassesWhenStatusesPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"build","state":"SUCCESSFUL"}]}`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected Bitbucket CI pass to complete without approval")
	}
	if api.prStateCalls == 0 {
		t.Fatal("expected Bitbucket PR state endpoint to be called")
	}
	if api.statusesCalls == 0 {
		t.Fatal("expected Bitbucket statuses endpoint to be called")
	}
	foundPassed := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			foundPassed = true
			break
		}
	}
	if !foundPassed {
		t.Fatalf("expected successful Bitbucket CI logs, got %v", logs)
	}
}

func TestCIStep_BitbucketUsesProcessEnvWhenStepEnvIsNil(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"build","state":"SUCCESSFUL"}]}`)
	t.Setenv("NO_MISTAKES_BITBUCKET_EMAIL", "test@example.com")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_TOKEN", "test-token")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_BASE_URL", api.server.URL)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected Bitbucket CI pass to complete without approval")
	}
	if api.prStateCalls == 0 || api.statusesCalls == 0 {
		t.Fatalf("expected Bitbucket CI endpoints to be called, got state=%d statuses=%d", api.prStateCalls, api.statusesCalls)
	}
}

func TestCIStep_BitbucketFailureNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"build","state":"FAILED"}]}`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected Bitbucket CI failures to require approval when auto-fix is disabled")
	}
	if api.prStateCalls == 0 || api.statusesCalls == 0 {
		t.Fatalf("expected Bitbucket CI endpoints to be called, got state=%d statuses=%d", api.prStateCalls, api.statusesCalls)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("expected failing Bitbucket check finding, got %+v", findings.Items)
	}
}

func TestCIStep_BitbucketStoppedDoesNotNeedApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"build","state":"STOPPED"}]}`)

	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Config.CITimeout = 30 * time.Second

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected stopped Bitbucket CI to avoid failure handling")
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

	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"test","state":"FAILED"}]}`)
	api.pipelinesJSON = `{"values":[{"uuid":"{pipeline-1}"}]}`
	api.stepsJSON = `{"values":[{"uuid":"{step-1}","state":{"name":"COMPLETED","result":{"name":"FAILED"}}}]}`
	api.stepLog = "error log output"

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
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

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
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after auto-fix poll, got %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "error log output") {
		t.Fatalf("expected Bitbucket auto-fix prompt to include pipeline logs, got:\n%s", capturedPrompt)
	}
	if api.pipelinesCalls == 0 || api.stepsCalls == 0 || api.stepLogCalls == 0 {
		t.Fatalf("expected Bitbucket pipeline log endpoints to be called, got pipelines=%d steps=%d log=%d", api.pipelinesCalls, api.stepsCalls, api.stepLogCalls)
	}
}

func TestCIStep_BitbucketAutoFixUsesLivePRHeadSHAForLogs(t *testing.T) {
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

	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"test","state":"FAILED"}]}`)
	api.pipelinesJSON = `{"values":[{"uuid":"{pipeline-1}"}]}`
	api.stepsJSON = `{"values":[{"uuid":"{step-1}","state":{"name":"COMPLETED","result":{"name":"FAILED"}}}]}`
	api.stepLog = "error log output"
	api.prSourceSHA = headSHA

	staleHeadSHA := strings.Repeat("a", 40)
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
	sctx := newTestContext(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

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
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after auto-fix poll, got %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if api.lastPipelineQ != headSHA {
		t.Fatalf("pipeline commit SHA = %q, want live PR source commit %q", api.lastPipelineQ, headSHA)
	}
	if api.lastPipelineQ == staleHeadSHA {
		t.Fatalf("pipeline lookup used stale run head SHA %q", staleHeadSHA)
	}
}

func TestCIStep_BitbucketAutoFixUsesMatchingPipelineLogs(t *testing.T) {
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

	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"name":"test","state":"FAILED","url":"https://bitbucket.org/test/repo/addon/pipelines/home#!/results/pipeline-2"}]}`)
	api.pipelinesJSON = `{"values":[{"uuid":"{pipeline-1}"},{"uuid":"{pipeline-2}"}]}`
	api.stepsByPath = map[string]string{
		"/2.0/repositories/test/repo/pipelines/{pipeline-1}/steps": `{"values":[{"uuid":"{step-1}","state":{"name":"COMPLETED","result":{"name":"FAILED"}}}]}`,
		"/2.0/repositories/test/repo/pipelines/{pipeline-2}/steps": `{"values":[{"uuid":"{step-2}","state":{"name":"COMPLETED","result":{"name":"FAILED"}}}]}`,
	}
	api.stepLogsByPath = map[string]string{
		"/2.0/repositories/test/repo/pipelines/{pipeline-1}/steps/{step-1}/log": "wrong pipeline log",
		"/2.0/repositories/test/repo/pipelines/{pipeline-2}/steps/{step-2}/log": "matching pipeline log",
	}
	api.prSourceSHA = headSHA

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
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

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
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after auto-fix poll, got %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected Bitbucket auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "matching pipeline log") {
		t.Fatalf("expected prompt to include matching pipeline log, got:\n%s", capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "wrong pipeline log") {
		t.Fatalf("expected prompt to exclude unrelated pipeline log, got:\n%s", capturedPrompt)
	}
	if api.stepLogCalls != 1 {
		t.Fatalf("expected exactly one Bitbucket step log fetch, got %d", api.stepLogCalls)
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

	got := latestBitbucketStatuses(statuses)
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

	got := latestBitbucketStatuses(statuses)
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

	api := newFakeBitbucketCIAPI(t, "OPEN", `{"values":[{"key":"build","state":"FAILED"}]}`)
	client, err := bitbucket.NewClientFromEnv(fakeBitbucketEnv(api.server.URL))
	if err != nil {
		t.Fatalf("new bitbucket client: %v", err)
	}

	dir := t.TempDir()
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})
	step := &CIStep{}

	checks, err := step.getCIChecks(sctx, scm.ProviderBitbucket, client, bitbucket.RepoRef{Workspace: "test", RepoSlug: "repo"}, "42")
	if err != nil {
		t.Fatalf("getCIChecks returned error: %v", err)
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
}
