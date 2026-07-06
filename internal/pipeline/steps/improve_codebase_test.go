package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestImproveCodebaseStep_ModeOffSkips(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent should not run when improve-codebase is off")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeOff

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected improve-codebase off mode to skip")
	}
}

func TestImproveCodebaseStep_ModeAlwaysRunsAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !opts.ReadOnly {
			t.Fatal("expected improve-codebase agent invocation to request read-only mode")
		}
		if !strings.Contains(opts.Prompt, "Run the local improve-codebase skill") {
			t.Fatalf("prompt did not invoke improve-codebase skill: %s", opts.Prompt)
		}
		if !strings.Contains(opts.Prompt, "no-mistakes pipeline gate mode") {
			t.Fatalf("prompt did not request pipeline gate mode: %s", opts.Prompt)
		}
		if !strings.Contains(opts.Prompt, "Do not edit files") {
			t.Fatal("expected read-only guardrail in prompt")
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected always mode to run")
	}
	if outcome.NeedsApproval {
		t.Fatal("expected clean findings not to need approval")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
}

func TestImproveCodebaseStep_PromptIncludesIgnorePatterns(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		for _, want := range []string{
			"ignore patterns: vendor/**, generated/**",
			"Exclude files and paths matched by ignore_patterns from findings.",
		} {
			if !strings.Contains(opts.Prompt, want) {
				t.Fatalf("prompt missing %q:\n%s", want, opts.Prompt)
			}
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways
	sctx.Config.IgnorePatterns = []string{"vendor/**", "generated/**"}

	if _, err := (&ImproveCodebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
}

func TestImproveCodebaseStep_CleansAgentWorktreeChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		mustWriteFile(t, filepath.Join(dir, "agent-artifact.md"), "should not survive\n")
		mustWriteFile(t, filepath.Join(dir, "file.txt"), "agent edit\n")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
}

func TestImproveCodebaseStep_CleansAfterRunContextCancelled(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		mustWriteFile(t, filepath.Join(dir, "agent-artifact.md"), "should not survive\n")
		cancel()
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Ctx = ctx
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-artifact.md")); !os.IsNotExist(err) {
		t.Fatalf("agent artifact stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_RestoresAgentHeadChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		mustWriteFile(t, filepath.Join(dir, "agent-commit.md"), "should not survive\n")
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "agent commit")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-commit.md")); !os.IsNotExist(err) {
		t.Fatalf("agent artifact stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_RestoresDetachedHeadWhenAgentChecksOutSameCommitBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "checkout", "feature")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("HEAD ref = %s, want detached", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
}

func TestImproveCodebaseStep_RestoresAgentLocalGitConfigChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "config", "--local", "core.fsmonitor", "agent-hook")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if got, err := git.Run(context.Background(), dir, "config", "--local", "--get", "core.fsmonitor"); err == nil {
		t.Fatalf("core.fsmonitor = %q, want unset", got)
	}
}

func TestImproveCodebaseStep_RestoresAgentIndexFlags(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-index", "--skip-worktree", "feature.txt")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if got := gitCmd(t, dir, "ls-files", "-v", "--", "feature.txt"); strings.HasPrefix(got, "S ") {
		t.Fatalf("index flag = %q, want skip-worktree cleared", got)
	}
}

func TestImproveCodebaseStep_CleansAgentIgnoredFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	mustWriteFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore logs")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		mustWriteFile(t, filepath.Join(dir, "agent.log"), "should not survive\n")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if status := gitCmd(t, dir, "status", "--porcelain", "--ignored"); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.log")); !os.IsNotExist(err) {
		t.Fatalf("ignored artifact stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_CleansNestedGitRepos(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		nested := filepath.Join(dir, "nested-repo")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, nested, "init")
		mustWriteFile(t, filepath.Join(nested, "nested.txt"), "artifact\n")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if _, err := os.Stat(filepath.Join(dir, "nested-repo")); !os.IsNotExist(err) {
		t.Fatalf("nested repo stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_RestoresProtectedRefsOnAgentError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	beforeLocalFeature := headSHA
	beforeOriginFeature := baseSHA
	beforeForkFeature := headSHA
	gitCmd(t, dir, "update-ref", "refs/heads/feature", beforeLocalFeature)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/feature", beforeOriginFeature)
	gitCmd(t, dir, "update-ref", forkBranchTrackingRef("feature"), beforeForkFeature)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-ref", "refs/heads/feature", baseSHA)
		gitCmd(t, dir, "update-ref", "refs/remotes/origin/feature", headSHA)
		gitCmd(t, dir, "update-ref", forkBranchTrackingRef("feature"), baseSHA)
		return nil, fmt.Errorf("agent failed")
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways
	sctx.Repo.ForkURL = "https://github.com/test/fork.git"

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("error = %v, want read-only violation", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != beforeLocalFeature {
		t.Fatalf("local feature = %s, want %s", got, beforeLocalFeature)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/remotes/origin/feature"); got != beforeOriginFeature {
		t.Fatalf("origin/feature = %s, want %s", got, beforeOriginFeature)
	}
	if got := gitCmd(t, dir, "rev-parse", forkBranchTrackingRef("feature")); got != beforeForkFeature {
		t.Fatalf("fork tracking ref = %s, want %s", got, beforeForkFeature)
	}
}

func TestImproveCodebaseStep_RemovesCreatedProtectedRef(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "remotes", "origin", "feature")); !os.IsNotExist(err) {
		t.Fatalf("origin/feature should start absent, stat err = %v", err)
	}

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-ref", "refs/remotes/origin/feature", headSHA)
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "remotes", "origin", "feature")); !os.IsNotExist(err) {
		t.Fatalf("created protected ref stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_RemovesCreatedSharedRefs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-ref", "refs/tags/agent-tag", headSHA)
		gitCmd(t, dir, "update-ref", "refs/replace/"+headSHA, baseSHA)
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "tags", "agent-tag")); !os.IsNotExist(err) {
		t.Fatalf("created tag stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "replace", headSHA)); !os.IsNotExist(err) {
		t.Fatalf("created replace ref stat err = %v, want not exist", err)
	}
}

func TestImproveCodebaseStep_RestoresChangedSharedRefs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	beforeTag := baseSHA
	gitCmd(t, dir, "update-ref", "refs/tags/release", beforeTag)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-ref", "refs/tags/release", headSHA)
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/tags/release"); got != beforeTag {
		t.Fatalf("tag = %s, want %s", got, beforeTag)
	}
}

func TestImproveCodebaseStep_RestoresRefsDespiteUnmergedIndex(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	beforeTag := baseSHA
	gitCmd(t, dir, "update-ref", "refs/tags/release", beforeTag)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "update-ref", "refs/tags/release", headSHA)
		gitCmd(t, dir, "checkout", "-b", "agent-side", headSHA)
		mustWriteFile(t, filepath.Join(dir, "base.txt"), "side\n")
		gitCmd(t, dir, "add", "base.txt")
		gitCmd(t, dir, "commit", "-m", "side edit")
		gitCmd(t, dir, "checkout", "feature")
		mustWriteFile(t, filepath.Join(dir, "base.txt"), "feature\n")
		gitCmd(t, dir, "add", "base.txt")
		gitCmd(t, dir, "commit", "-m", "feature edit")
		if _, err := git.Run(context.Background(), dir, "merge", "agent-side"); err == nil {
			t.Fatal("expected merge conflict")
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/tags/release"); got != beforeTag {
		t.Fatalf("tag = %s, want %s", got, beforeTag)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
}

func TestImproveCodebaseStep_RestoresRetargetedSymbolicRef(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", headSHA)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/dev", headSHA)
	gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/dev")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if got := gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
		t.Fatalf("origin/HEAD = %s, want refs/remotes/origin/main", got)
	}
}

func TestImproveCodebaseStep_RemovesCreatedSymbolicRef(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", headSHA)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if _, err := git.Run(context.Background(), dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		t.Fatal("origin/HEAD still exists, want removed")
	}
}

func TestImproveCodebaseStep_DetachesBeforeRefRestore(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	beforeLocalFeature := headSHA
	gitCmd(t, dir, "update-ref", "refs/heads/feature", beforeLocalFeature)

	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "checkout", "feature")
		mustWriteFile(t, filepath.Join(dir, "branch-edit.txt"), "branch edit\n")
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "agent branch commit")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	_, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected read-only violation")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != beforeLocalFeature {
		t.Fatalf("local feature = %s, want %s", got, beforeLocalFeature)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want clean after cleanup", status)
	}
}

func TestImproveCodebaseStep_AutoSkipsSmallChange(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent should not run for a small isolated text change")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected small auto-mode change to skip")
	}
	if !strings.Contains(outcome.Findings, "small and not structurally risky") {
		t.Fatalf("findings = %q, want skip reason", outcome.Findings)
	}
}

func TestImproveCodebaseStep_AutoRunsForCrossDirectoryMove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	mustWriteFile(t, filepath.Join(dir, "internal", "api", "client.go"), strings.Repeat("package api\n\nfunc clientMarker() string { return \"client\" }\n", 4))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add api client")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "mv", "internal/api/client.go", "pkg/client/client.go")
	gitCmd(t, dir, "commit", "-m", "move client")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, "file moved across directories") {
			t.Fatalf("prompt missing trigger reason: %s", opts.Prompt)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected cross-directory move to run")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
}

func TestImproveCodebaseStep_AutoRunsForManySourceFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	for i := 0; i < improveCodebaseSourceFileThreshold+1; i++ {
		mustWriteFile(t, filepath.Join(dir, "pkg", "many", fmt.Sprintf("file%02d.go", i)), "package many\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "touch many files")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, "source files changed") {
			t.Fatalf("prompt missing source-file trigger reason: %s", opts.Prompt)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected many source files to run")
	}
}

func TestImproveCodebaseStep_BlockingFindingsNeedApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findings := Findings{
		Items: []Finding{{
			ID:          "ic-1",
			Severity:    "warning",
			File:        "internal/api/client.go",
			Description: "new adapter boundary duplicates existing provider mechanics",
			Action:      "ask-user",
		}},
		Summary: "1 structural warning",
	}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected warning finding to need approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected improve-codebase gate not to be auto-fixable")
	}
	if !outcome.DisableFix {
		t.Fatal("expected improve-codebase gate to disable manual fix")
	}
}

func TestImproveCodebaseStep_NormalizesAuditOnlyActions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findings := Findings{
		Items: []Finding{
			{ID: "ic-1", Severity: "warning", Description: "structural issue", Action: types.ActionAutoFix},
			{ID: "ic-2", Severity: "info", Description: "note", Action: types.ActionAutoFix},
		},
		Summary: "mixed actions",
	}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(string(opts.JSONSchema), `"enum": ["no-op", "ask-user"]`) {
			t.Fatalf("expected audit-only findings schema, got %s", opts.JSONSchema)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	var got Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &got); err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Action != types.ActionAskUser {
		t.Fatalf("warning action = %q, want %q", got.Items[0].Action, types.ActionAskUser)
	}
	if got.Items[1].Action != types.ActionNoOp {
		t.Fatalf("info action = %q, want %q", got.Items[1].Action, types.ActionNoOp)
	}
}

func TestImproveCodebaseSourceFileIgnoresGeneratedAndVendoredPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"vendor/example/client.go",
		"node_modules/example/index.ts",
		"internal/vendor/example/client.go",
		"web/node_modules/example/index.ts",
		"internal/api/client_generated.go",
		"internal/api/client.pb.go",
	} {
		if isImproveCodebaseSourceFile(path) {
			t.Errorf("%s should not count as an improve-codebase source file", path)
		}
	}
	if !isImproveCodebaseSourceFile("internal/api/client.go") {
		t.Error("ordinary source file should count")
	}
}

func TestImproveCodebaseHighRiskPathIncludesNoMistakesConfig(t *testing.T) {
	t.Parallel()
	if !isImproveCodebaseHighRiskPath(".no-mistakes.yaml") {
		t.Fatal(".no-mistakes.yaml should trigger improve-codebase auto mode")
	}
}

func TestAllStepsIncludesImproveCodebaseAfterReview(t *testing.T) {
	t.Parallel()
	steps := AllSteps()
	var got []types.StepName
	for _, step := range steps {
		got = append(got, step.Name())
	}
	want := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepImproveCodebase,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
