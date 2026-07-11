package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

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
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{
					Output: json.RawMessage(`{"findings":[],"summary":"conflict resolution verified"}`),
				}, nil
			}
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
	sctx.UserIntent = "user wanted conflict resolution to preserve the extracted intent"

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after successful fix")
	}
	if len(ag.calls) != 2 {
		t.Fatalf("expected 2 agent calls (fixer + verifier), got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[1].Prompt, "independently verifying") {
		t.Fatalf("expected second call to be the independent verifier, got: %s", ag.calls[1].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "shared.txt") {
		t.Error("expected agent prompt to mention conflicting file")
	}
	if strings.Contains(ag.calls[0].Prompt, "other.txt") && !strings.Contains(ag.calls[0].Prompt, "Current conflicted files") {
		t.Fatalf("expected prompt to scope fixes using current conflicted files, got: %s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "user wanted conflict resolution to preserve the extracted intent") {
		t.Fatalf("expected agent prompt to include extracted user intent, got: %s", ag.calls[0].Prompt)
	}
	// Verify rebase completed - feature is now ahead of origin/main
	mergeBase := gitCmd(t, dir, "merge-base", "HEAD", "origin/main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	if mergeBase != originMain {
		t.Fatalf("merge-base = %s, want origin/main %s", mergeBase, originMain)
	}
}

// setupRebaseConflictRepo builds a repo where feature and origin/main both edit
// shared.txt, so rebasing feature onto origin/main conflicts.
func setupRebaseConflictRepo(t *testing.T) (dir, upstream, baseSHA, headSHA string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")
	return dir, upstream, baseSHA, headSHA
}

// resolveConflictContinue writes a merged shared.txt, stages it, and completes
// the in-progress rebase, mirroring what a conflict-repair fixer does.
func resolveConflictContinue(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved content\n"), 0o644); err != nil {
		return err
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_EDITOR=true",
	)
	add := exec.Command("git", "add", "shared.txt")
	add.Dir = dir
	add.Env = env
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s: %w", out, err)
	}
	cont := exec.Command("git", "rebase", "--continue")
	cont.Dir = dir
	cont.Env = env
	if out, err := cont.CombinedOutput(); err != nil {
		return fmt.Errorf("git rebase --continue: %s: %w", out, err)
	}
	return nil
}

// TestRebaseStep_FixModeEscalatesThenResolves proves the conflict repair climbs
// from fix_balanced to authority_strong: the first tier leaves the rebase
// unresolved, the loop aborts and retries at the next tier, which resolves.
func TestRebaseStep_FixModeEscalatesThenResolves(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			fixerCalls++
			if fixerCalls == 1 {
				// Tier 0 (fix_balanced): fail to complete the rebase.
				return &agent.Result{Output: json.RawMessage(`{"summary":"could not resolve"}`)}, nil
			}
			// Tier 1 (authority_strong): resolve and continue.
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig() // enables the fix_balanced->authority_strong cascade
	sctx.Fixing = true

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after escalated resolution")
	}
	if fixerCalls != 2 {
		t.Fatalf("expected 2 fixer calls (tier 0 failure -> tier 1), got %d", fixerCalls)
	}
	mergeBase := gitCmd(t, dir, "merge-base", "HEAD", "origin/main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	if mergeBase != originMain {
		t.Fatalf("merge-base = %s, want origin/main %s (rebase not completed)", mergeBase, originMain)
	}
}

func TestRebaseStep_FixModeStopsWhenProfileIsUnavailable(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)

	fixerCalls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			fixerCalls++
			return nil, fmt.Errorf("route invocation failed: %w", &agent.ProfileUnavailableError{
				Profile: "fix_balanced",
				Cause:   errors.New("all providers unavailable"),
			})
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Routing = config.DefaultRoutingConfig()
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected unavailable fix_balanced profile to terminate conflict repair")
	}
	if fixerCalls != 1 {
		t.Fatalf("fixer calls = %d, want 1 with no authority_strong tier jump", fixerCalls)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want pre-rebase %s", got, headSHA)
	}
	if status := gitCmd(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("expected clean worktree after unavailable profile, got: %s", status)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

// TestRebaseStep_FixModeFailsClosedOnVerifierRejection proves a completed
// resolution that the independent verifier rejects fails closed and unwinds the
// worktree to the pre-rebase HEAD rather than recording the branch update.
func TestRebaseStep_FixModeFailsClosedOnVerifierRejection(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","file":"shared.txt","description":"dropped upstream change"}],"summary":"incorrect resolution"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	step := &RebaseStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected a fail-closed error when the verifier rejects the resolution")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want pre-rebase %s (worktree not unwound)", got, headSHA)
	}
	if status := gitCmd(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("expected clean worktree after fail-closed unwind, got: %s", status)
	}
}

func TestRebaseStep_FixModeRequiresConclusiveVerifierOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		output     string
		wantReject bool
	}{
		{name: "empty object", output: `{}`, wantReject: true},
		{name: "missing findings", output: `{"summary":"verified"}`, wantReject: true},
		{name: "missing summary", output: `{"findings":[]}`, wantReject: true},
		{
			name:       "unresolved and inconclusive",
			output:     `{"findings":[{"severity":"info","description":"resolution remains unresolved and could not be conclusively verified","action":"no-op"}],"summary":"inconclusive"}`,
			wantReject: true,
		},
		{name: "valid control", output: `{"findings":[],"summary":"conflict resolution verified"}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)

			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if strings.Contains(opts.Prompt, "independently verifying") {
						return &agent.Result{Output: json.RawMessage(tt.output)}, nil
					}
					if err := resolveConflictContinue(dir); err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
				},
			}

			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = upstream
			sctx.Fixing = true

			_, err := (&RebaseStep{}).Execute(sctx)
			if tt.wantReject {
				if err == nil {
					t.Fatal("expected verifier output to fail closed")
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
					t.Fatalf("HEAD = %s, want pre-rebase %s", got, headSHA)
				}
				if sctx.Run.HeadSHA != headSHA {
					t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
				}
			} else {
				if err != nil {
					t.Fatalf("valid verifier output failed: %v", err)
				}
				currentHead := gitCmd(t, dir, "rev-parse", "HEAD")
				if currentHead == headSHA {
					t.Fatal("valid verifier output did not preserve the completed rebase")
				}
				if sctx.Run.HeadSHA != currentHead {
					t.Fatalf("Run.HeadSHA = %s, want rebased HEAD %s", sctx.Run.HeadSHA, currentHead)
				}
			}
			if status := gitCmd(t, dir, "status", "--porcelain"); status != "" {
				t.Fatalf("expected clean worktree, got: %s", status)
			}
			storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedRun.HeadSHA != sctx.Run.HeadSHA {
				t.Fatalf("stored Run.HeadSHA = %s, want %s", storedRun.HeadSHA, sctx.Run.HeadSHA)
			}
		})
	}
}

func TestRebaseStep_FixModeRejectsVerifierCommitWithoutHidingIt(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)

	var fixerCandidateSHA, verifierCommitSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				fixerCandidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(dir, "verifier.txt"), []byte("verifier mutation\n"), 0o644); err != nil {
					return nil, err
				}
				gitCmd(t, dir, "add", "--", "verifier.txt")
				gitCmd(t, dir, "commit", "-m", "verifier mutation")
				verifierCommitSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier-created commit to fail closed")
	}
	if fixerCandidateSHA == "" || fixerCandidateSHA == headSHA {
		t.Fatalf("fixer candidate HEAD = %q, want completed rebased candidate", fixerCandidateSHA)
	}
	if verifierCommitSHA == "" || verifierCommitSHA == fixerCandidateSHA {
		t.Fatalf("verifier commit HEAD = %q, want commit after fixer candidate %s", verifierCommitSHA, fixerCandidateSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != verifierCommitSHA {
		t.Fatalf("HEAD = %s, want visible verifier mutation %s", got, verifierCommitSHA)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

func TestRebaseStep_FixModeRejectsVerifierDirtyWorktreeWithoutHidingIt(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "candidate-untracked.txt"), []byte("fixer candidate content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fixerCandidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				fixerCandidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(dir, "candidate-untracked.txt"), []byte("verifier changed untracked content\n"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier worktree mutation to fail closed")
	}
	if fixerCandidateSHA == "" || fixerCandidateSHA == headSHA {
		t.Fatalf("fixer candidate HEAD = %q, want completed rebased candidate", fixerCandidateSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != fixerCandidateSHA {
		t.Fatalf("HEAD = %s, want visible fixer candidate %s", got, fixerCandidateSHA)
	}
	status := gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all")
	if !strings.Contains(status, "candidate-untracked.txt") {
		t.Fatalf("expected verifier mutation to remain visible, got status:\n%s", status)
	}
	content, err := os.ReadFile(filepath.Join(dir, "candidate-untracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "verifier changed untracked content\n" {
		t.Fatalf("untracked candidate content = %q, want visible verifier mutation", got)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

func TestRebaseStep_FixModeRejectsVerifierUntrackedExecutableBitMutationWithoutHidingIt(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	candidatePath := filepath.Join(dir, "candidate-untracked.sh")
	if err := os.WriteFile(candidatePath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(candidatePath, 0o755); err != nil {
		t.Skipf("executable-bit setup unavailable: %v", err)
	}
	info, err := os.Lstat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Skip("filesystem does not preserve executable bits")
	}
	if err := os.Chmod(candidatePath, 0o644); err != nil {
		t.Fatal(err)
	}

	var fixerCandidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				fixerCandidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := os.Chmod(candidatePath, 0o755); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier executable-bit mutation to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != fixerCandidateSHA {
		t.Fatalf("HEAD = %s, want visible fixer candidate %s", got, fixerCandidateSHA)
	}
	info, err = os.Lstat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("untracked candidate mode = %o, want visible verifier executable bit", info.Mode().Perm())
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

func TestRebaseStep_FixModeRejectsVerifierUntrackedSymlinkPayloadMutationWithoutHidingIt(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	for _, name := range []string{"candidate-target-a.txt", "candidate-target-b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("same target content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	candidatePath := filepath.Join(dir, "candidate-untracked-link")
	if err := os.Symlink("candidate-target-a.txt", candidatePath); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	var fixerCandidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				fixerCandidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := os.Remove(candidatePath); err != nil {
					return nil, err
				}
				if err := os.Symlink("candidate-target-b.txt", candidatePath); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier symlink-payload mutation to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != fixerCandidateSHA {
		t.Fatalf("HEAD = %s, want visible fixer candidate %s", got, fixerCandidateSHA)
	}
	target, err := os.Readlink(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if target != "candidate-target-b.txt" {
		t.Fatalf("untracked candidate symlink target = %q, want visible verifier target", target)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

func TestRebaseStep_FixModeRejectsVerifierUntrackedFileTypeMutationWithoutHidingIt(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	const payload = "same candidate content\n"
	targetName := "candidate-target.txt"
	if err := os.WriteFile(filepath.Join(dir, targetName), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(dir, "candidate-untracked")
	if err := os.WriteFile(candidatePath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(dir, ".candidate-symlink-probe")
	if err := os.Symlink(targetName, probePath); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}
	if err := os.Remove(probePath); err != nil {
		t.Fatal(err)
	}

	var fixerCandidateSHA string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				fixerCandidateSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := os.Remove(candidatePath); err != nil {
					return nil, err
				}
				if err := os.Symlink(targetName, candidatePath); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected verifier file-type mutation to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != fixerCandidateSHA {
		t.Fatalf("HEAD = %s, want visible fixer candidate %s", got, fixerCandidateSHA)
	}
	info, err := os.Lstat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("untracked candidate mode = %s, want visible verifier symlink", info.Mode())
	}
	target, err := os.Readlink(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if target != targetName {
		t.Fatalf("untracked candidate symlink target = %q, want %q", target, targetName)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("Run.HeadSHA = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != headSHA {
		t.Fatalf("stored Run.HeadSHA = %s, want unchanged %s", storedRun.HeadSHA, headSHA)
	}
}

func TestRebaseStep_FixModeAcceptsCleanVerifierWithUntrackedModesAndSymlinks(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupRebaseConflictRepo(t)
	executablePath := filepath.Join(dir, "candidate-untracked.sh")
	if err := os.WriteFile(executablePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executableInfo, err := os.Lstat(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if executableInfo.Mode().Perm()&0o111 == 0 {
		t.Skip("filesystem does not preserve executable bits")
	}
	targetName := "candidate-target.txt"
	if err := os.WriteFile(filepath.Join(dir, targetName), []byte("target content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "candidate-untracked-link")
	if err := os.Symlink(targetName, linkPath); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "independently verifying") {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"verified"}`)}, nil
			}
			if err := resolveConflictContinue(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("clean verifier failed: %v", err)
	}
	currentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if currentHead == headSHA {
		t.Fatal("clean verifier did not preserve the completed rebase")
	}
	if sctx.Run.HeadSHA != currentHead {
		t.Fatalf("Run.HeadSHA = %s, want rebased HEAD %s", sctx.Run.HeadSHA, currentHead)
	}
	info, err := os.Lstat(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("untracked executable mode = %o, want executable bit", info.Mode().Perm())
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != targetName {
		t.Fatalf("untracked candidate symlink target = %q, want %q", target, targetName)
	}
	storedRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.HeadSHA != currentHead {
		t.Fatalf("stored Run.HeadSHA = %s, want rebased HEAD %s", storedRun.HeadSHA, currentHead)
	}
}

func TestRebaseStep_ForkSyncsPushBranchBeforeDefaultBranch(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	fork := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", parent)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", fork, "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "push", fork, "feature")

	if err := os.WriteFile(filepath.Join(dir, "fork.txt"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fork update")
	forkOnlySHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", fork, "feature")

	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "local update")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("unexpected approval after clean fork rebase: %s", outcome.Findings)
	}
	if _, err := os.Stat(filepath.Join(dir, "fork.txt")); err != nil {
		t.Fatalf("expected fork-only commit to be included after rebase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.txt")); err != nil {
		t.Fatalf("expected local commit to remain after rebase: %v", err)
	}
	if mergeBase := gitCmd(t, dir, "merge-base", "HEAD", forkOnlySHA); mergeBase != forkOnlySHA {
		t.Fatalf("merge-base = %s, want fork tip %s", mergeBase, forkOnlySHA)
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
