package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRebaseStep_ConflictReturnsFindings(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch: modify shared.txt
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Main branch: conflicting change to shared.txt
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval for conflict")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected AutoFixable for conflict")
	}
	if outcome.Findings == "" {
		t.Fatal("expected findings for conflict")
	}
	if !strings.Contains(outcome.Findings, "origin/main") {
		t.Errorf("expected findings to mention conflict target, got: %s", outcome.Findings)
	}
	// Verify repo is clean (rebase was aborted)
	status := gitStatusPorcelain(t, dir)
	if status != "" {
		t.Fatalf("expected clean worktree after abort, got: %s", status)
	}
	// Verify no agent calls in detection mode
	if len(ag.calls) != 0 {
		t.Errorf("expected 0 agent calls, got %d", len(ag.calls))
	}
}

func TestRebaseStep_ConflictTriesAllTargets(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Create feature branch, push it to origin
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature-origin\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature origin change")
	gitCmd(t, dir, "push", "origin", "feature")

	// Diverge local feature from origin/feature (conflicting change to shared.txt)
	gitCmd(t, dir, "reset", "--soft", "HEAD~1")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature-local\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature local change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main with a non-conflicting change, push
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("main update\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main non-conflicting update")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval for conflict")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected AutoFixable for conflict")
	}
	if !strings.Contains(outcome.Findings, "origin/feature") {
		t.Errorf("expected findings to mention conflict target, got: %s", outcome.Findings)
	}

	// The non-conflicting rebase onto origin/main should have succeeded
	logOutput := gitCmd(t, dir, "log", "--oneline", "--all")
	if !strings.Contains(logOutput, "main non-conflicting update") {
		t.Log("git log:\n" + logOutput)
	}
	// Verify HEAD includes the main update (rebase onto origin/main applied)
	headLog := gitCmd(t, dir, "log", "--oneline")
	if !strings.Contains(headLog, "main non-conflicting update") {
		t.Errorf("expected HEAD to include the origin/main rebase; git log:\n%s", headLog)
	}

	// Verify worktree is clean
	status := gitStatusPorcelain(t, dir)
	if status != "" {
		t.Fatalf("expected clean worktree, got: %s", status)
	}
}

func TestRebaseStep_ConflictFindingsIncludeFiles(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) == 0 {
		t.Fatal("expected conflicted files in findings")
	}
	if findings.Items[0].File != "shared.txt" {
		t.Fatalf("first finding file = %q, want shared.txt", findings.Items[0].File)
	}
	if findings.Items[0].Severity != "warning" {
		t.Fatalf("first finding severity = %q, want warning", findings.Items[0].Severity)
	}
	if !strings.Contains(findings.Items[0].Description, "origin/main") {
		t.Fatalf("expected finding description to mention target, got %q", findings.Items[0].Description)
	}
}

func TestRebaseStep_FixModeCallsAgent(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	// Agent simulates resolving conflicts: resolve file, git add, git rebase --continue
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Resolve the conflict by writing the merged content
			os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved content\n"), 0o644)
			cmd := exec.Command("git", "add", "shared.txt")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git add: %s: %w", out, err)
			}
			cmd = exec.Command("git", "rebase", "--continue")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
				"GIT_EDITOR=true",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git rebase --continue: %s: %w", out, err)
			}
			return &agent.Result{
				Output: json.RawMessage(`{"summary":"resolve merge conflict in shared.txt"}`),
			}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","file":"other.txt","description":"merge conflict rebasing onto origin/feature"}]}`

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after successful fix")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "shared.txt") {
		t.Error("expected agent prompt to mention conflicting file")
	}
	if strings.Contains(ag.calls[0].Prompt, "other.txt") && !strings.Contains(ag.calls[0].Prompt, "Current conflicted files") {
		t.Fatalf("expected prompt to scope fixes using current conflicted files, got: %s", ag.calls[0].Prompt)
	}
	// Verify rebase completed - feature is now ahead of origin/main
	mergeBase := gitCmd(t, dir, "merge-base", "HEAD", "origin/main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	if mergeBase != originMain {
		t.Fatalf("merge-base = %s, want origin/main %s", mergeBase, originMain)
	}
}

func TestRebaseStep_FixModeNonConflictFailureReturnsError(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main so rebase is needed
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("main\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main advance")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	// Dirty the working tree so rebase fails without conflict
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("dirty\n"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	step := &RebaseStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error for non-conflict rebase failure")
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected 0 agent calls for non-conflict failure, got %d", len(ag.calls))
	}
}

func TestRebaseStep_NonConflictFailureWithRebaseMetadataReturnsError(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("main\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main advance")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("dirty\n"), 0o644)
	rebaseMergeDir := gitCmd(t, dir, "rev-parse", "--git-path", "rebase-merge")
	if err := os.MkdirAll(rebaseMergeDir, 0o755); err != nil {
		t.Fatalf("mkdir rebase metadata: %v", err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error for non-conflict rebase failure")
	}
	if outcome != nil {
		t.Fatalf("expected no outcome on error, got %#v", outcome)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected 0 agent calls, got %d", len(ag.calls))
	}
	if strings.Contains(gitStatusPorcelain(t, dir), "UU") {
		t.Fatal("expected no unmerged files")
	}
}

func TestRebaseStep_LogFileNotVisibleToUser(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "init")
	sha := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch with no upstream ref (will trigger fetch warning)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, sha, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	var userLogs []string
	var fileLogs []string
	sctx.Log = func(s string) { userLogs = append(userLogs, s) }
	sctx.LogFile = func(s string) { fileLogs = append(fileLogs, s) }

	step := &RebaseStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch warnings should go to file only, not user
	for _, log := range userLogs {
		if strings.Contains(log, "could not fetch") {
			t.Errorf("fetch warning leaked to user logs: %s", log)
		}
	}
	hasFileWarning := false
	for _, log := range fileLogs {
		if strings.Contains(log, "could not fetch") {
			hasFileWarning = true
		}
	}
	if !hasFileWarning {
		t.Error("expected fetch warning in file logs")
	}
}
