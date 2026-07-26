package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestLoadRecoveredConfig_BoundsFetchAndFailsClosed(t *testing.T) {
	oldTimeout := recoveredConfigFetchTimeout
	recoveredConfigFetchTimeout = 20 * time.Millisecond
	t.Cleanup(func() { recoveredConfigFetchTimeout = oldTimeout })

	fetchResult := make(chan error, 1)
	oldFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(ctx context.Context, _, _, _ string) error {
		select {
		case <-ctx.Done():
			fetchResult <- ctx.Err()
			return ctx.Err()
		case <-time.After(time.Second):
			err := errors.New("fetch context was not bounded")
			fetchResult <- err
			return err
		}
	}
	t.Cleanup(func() { fetchRecoveredRemoteBranch = oldFetch })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	gitCmd(t, workDir, "init", "--initial-branch=main")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	gitCmd(t, workDir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("commands:\n  lint: echo pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".no-mistakes.yaml")
	gitCmd(t, workDir, "commit", "-m", "candidate config")
	submittedSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	mgr := NewRunManager(nil, p, nil)
	started := time.Now()
	// The bounded fetch times out, leaving trustedSHA empty. Under the
	// disable_project_settings security boundary a trusted-config fetch failure
	// must ABORT (not silently proceed as "not opted out"), so this now returns
	// an error rather than a config with empty commands.
	_, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run", HeadSHA: submittedSHA, SubmittedHeadSHA: &submittedSHA}, &db.Repo{DefaultBranch: "main"}, workDir)
	if err == nil {
		t.Fatal("expected loadRecoveredConfig to abort on trusted-config fetch failure")
	}
	if !strings.Contains(err.Error(), "disable_project_settings") {
		t.Fatalf("abort error should name the boundary, got: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("load recovered config took %s, want under 1s", elapsed)
	}
	if err := <-fetchResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error = %v, want deadline exceeded", err)
	}
}

// TestLoadTrustedRepoConfig_FailClosedOnFetchFailure is the regression test for
// the trusted gate-boundary fix: when the default-branch fetch fails, startRun
// passes an empty trustedSHA, and loadTrustedRepoConfig MUST return nil even
// though a stale origin/<default> ref remains in the shared refs. Agent,
// document, and project-setting boundaries must never borrow that stale copy.
func TestLoadTrustedRepoConfig_FailClosedOnFetchFailure(t *testing.T) {
	ctx := context.Background()

	// Source repo whose default branch carries stale trusted config.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-command\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command on default branch")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	// The gate bare repo is its own origin so the linked worktree can fetch
	// main exactly the way startRun does.
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Linked worktree sharing the bare repo's refs and config.
	wt := filepath.Join(t.TempDir(), "wt")
	headSHA := gitOutput(t, src, "rev-parse", "HEAD")
	if err := git.WorktreeAdd(ctx, bare, wt, headSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// A previous successful fetch left origin/main present in the shared
	// refs — this is the stale ref the old code read after a fetch failure.
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("prime origin/main: %v", err)
	}
	ok, err := git.RefExists(ctx, wt, "origin/main")
	if err != nil {
		t.Fatalf("RefExists origin/main: %v", err)
	}
	if !ok {
		t.Fatal("precondition failed: origin/main should be present (the stale ref)")
	}

	// THE REGRESSION: fetch "failed" → startRun passes an empty trustedSHA.
	// Even with origin/main present, the trusted config must be nil.
	got := loadTrustedRepoConfig(ctx, wt, "", "test-run")
	if got != nil {
		t.Fatalf("expected nil trusted config on empty SHA (fetch failure); got commands.lint=%q", got.Commands.Lint)
	}

	// Candidate commands are independent run-owned state and survive this
	// value-level merge; startRun still aborts separately because the trusted
	// security boundaries could not be evaluated.
	pushed := &config.RepoConfig{
		Agent:    "pushed-agent",
		Commands: config.Commands{Lint: "echo pushed-branch-command"},
	}
	eff := config.EffectiveRepoConfig(pushed, got, false)
	if eff.Commands.Lint != "echo pushed-branch-command" {
		t.Fatalf("candidate command was replaced after trusted fetch failure: %q", eff.Commands.Lint)
	}
	if eff.Agent != "" {
		t.Fatalf("pushed agent should remain disabled without trusted config: %q", eff.Agent)
	}
}

// TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch proves the
// complementary side of review item #1: when the fetch succeeds, the trusted
// config is read at the exact resolved SHA (not the origin/<default> ref
// name), so it reflects the freshly fetched default-branch tip rather than a
// stale ref value. Advancing the default branch and re-fetching must yield the
// new command, not the old one.
func TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch(t *testing.T) {
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-A\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command A")
	staleSHA := gitOutput(t, src, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Advance the default branch to a fresh command and push.
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo fresh-B\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "fresh command B")
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")
	freshSHA := gitOutput(t, src, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, staleSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	resolved, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	if resolved != freshSHA {
		t.Fatalf("resolved SHA %s != fresh default-branch tip %s", resolved, freshSHA)
	}

	trusted := loadTrustedRepoConfig(ctx, wt, resolved, "test-run")
	if trusted == nil {
		t.Fatal("expected trusted config at the pinned fresh SHA")
	}
	if trusted.Commands.Lint != "echo fresh-B" {
		t.Fatalf("trusted lint = %q, want fresh-B (read at pinned SHA, not stale ref)", trusted.Commands.Lint)
	}
}
