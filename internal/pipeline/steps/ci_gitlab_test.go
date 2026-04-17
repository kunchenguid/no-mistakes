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
)

func TestCIStep_GitLabPassesWhenJobsPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"},{"id":2,"name":"test","status":"success"}]`
	env := fakeCIGlab(t, "opened", checksJSON)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected passing GitLab CI to complete without approval")
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
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
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGlab(t, "merged", "[]")

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}
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
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"},{"id":2,"name":"test","status":"failed"}]`
	env := fakeCIGlab(t, "opened", checksJSON)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected GitLab CI failure to require approval when auto-fix is disabled")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "test") {
		t.Fatalf("expected failing 'test' check finding, got %+v", findings.Items)
	}
}

func TestCIStep_GitLabMergeConflictDetected(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"id":1,"name":"build","status":"success"}]`
	env := fakeCIGlabConflict(t, "opened", checksJSON, true)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected merge conflict to require approval")
	}

	var findings Findings
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

	checksJSON := `[{"id":99,"name":"test","status":"failed"}]`
	env := fakeCIGlabWithTrace(t, "opened", checksJSON, "stack trace output from gitlab job")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "gitlab-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
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
		t.Fatal("expected GitLab auto-fix to call the agent")
	}
	if !strings.Contains(capturedPrompt, "CI logs:") || !strings.Contains(capturedPrompt, "stack trace output from gitlab job") {
		t.Fatalf("expected GitLab auto-fix prompt to include job trace, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_GitLabPendingChecksExitCleanlyWhenDone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	sequence := []string{
		`[{"id":1,"name":"build","status":"running"}]`,
		`[{"id":1,"name":"build","status":"success"}]`,
	}
	env := fakeCIGlabSequence(t, "opened", sequence)

	prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

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
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected passing GitLab CI to exit cleanly")
	}
	if pollCount != 1 {
		t.Fatalf("expected one poll wait while jobs were pending, got %d", pollCount)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "all CI checks passed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'all CI checks passed' log, got: %v", logs)
	}
}
