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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
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
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("commands:\n  lint: echo pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(nil, p, nil)
	started := time.Now()
	cfg, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run"}, &db.Repo{DefaultBranch: "main"}, workDir)
	if err != nil {
		t.Fatalf("load recovered config: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("load recovered config took %s, want under 1s", elapsed)
	}
	if err := <-fetchResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error = %v, want deadline exceeded", err)
	}
	if cfg.Commands.Lint != "" {
		t.Fatalf("commands.lint = %q, want empty after fetch timeout", cfg.Commands.Lint)
	}
}

// TestLoadTrustedRepoConfig_FailClosedOnFetchFailure is the regression test for
// the supply-chain RCE review item #1: when the default-branch fetch fails,
// startRun passes an empty trustedSHA, and loadTrustedRepoConfig MUST return
// nil even though a (potentially stale) origin/<default> ref is still present
// in the worktree's shared refs. Reading that stale ref would run a command
// the live default branch has already removed. EffectiveRepoConfig then forces
// empty commands, so the stale command does not run.
func TestLoadTrustedRepoConfig_FailClosedOnFetchFailure(t *testing.T) {
	ctx := context.Background()

	// Source repo whose default branch carries a "stale" lint command — the
	// kind of command a maintainer has since removed but a stale ref would
	// still serve.
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
	// Even with origin/main present and carrying the stale command, the
	// trusted config must be nil so the stale command cannot run.
	got, err := loadTrustedRepoConfig(ctx, wt, "", "test-run")
	if err != nil {
		t.Fatalf("loadTrustedRepoConfig: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil trusted config on empty SHA (fetch failure); got commands.lint=%q", got.Commands.Lint)
	}

	// And the effective config drops the pushed-branch command too — the
	// secure default, not a fallback to a stale or hostile copy.
	pushed := &config.RepoConfig{Commands: config.Commands{Lint: "echo pushed-branch-command"}}
	eff := config.EffectiveRepoConfig(pushed, got)
	if eff.Commands.Lint != "" {
		t.Fatalf("SECURITY REGRESSION: command would run after fetch failure: %q", eff.Commands.Lint)
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

	trusted, err := loadTrustedRepoConfig(ctx, wt, resolved, "test-run")
	if err != nil {
		t.Fatalf("loadTrustedRepoConfig: %v", err)
	}
	if trusted == nil {
		t.Fatal("expected trusted config at the pinned fresh SHA")
	}
	if trusted.Commands.Lint != "echo fresh-B" {
		t.Fatalf("trusted lint = %q, want fresh-B (read at pinned SHA, not stale ref)", trusted.Commands.Lint)
	}
}

func TestStartRunRejectsPresentInvalidTrustedConfigBeforeStepLaunch(t *testing.T) {
	ctx := context.Background()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "init", "--initial-branch=main")
	gitCmd(t, work, "config", "user.email", "test@test.com")
	gitCmd(t, work, "config", "user.name", "Test")
	gitCmd(t, work, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("routs:\n  initial_review: review_strong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-m", "invalid trusted config")
	mainSHA := gitOutput(t, work, "rev-parse", "HEAD")

	gitCmd(t, work, "switch", "-c", "feature")
	if err := os.Remove(filepath.Join(work, ".no-mistakes.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-m", "valid feature")
	featureSHA := gitOutput(t, work, "rev-parse", "HEAD")

	bare := p.RepoDir("invalid-trusted")
	gitCmd(t, "", "init", "--bare", bare)
	gitCmd(t, work, "remote", "add", "gate", bare)
	gitCmd(t, work, "push", "gate", "main:refs/heads/main", "feature:refs/heads/feature")
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin: %v", err)
	}
	repo, err := database.InsertRepoWithID("invalid-trusted", work, bare, "main")
	if err != nil {
		t.Fatal(err)
	}

	stepsRequested := false
	manager := NewRunManager(database, p, func() []pipeline.Step {
		stepsRequested = true
		return nil
	})
	_, err = manager.startRun(ctx, repo, "feature", featureSHA, mainSHA, "push", nil, "")
	if err == nil {
		t.Fatal("startRun accepted an invalid trusted default-branch config")
	}
	if !strings.Contains(err.Error(), "trusted repo config") || !strings.Contains(err.Error(), "routs") {
		t.Fatalf("startRun error = %v, want invalid trusted config and unknown key", err)
	}
	if stepsRequested {
		t.Fatal("startRun requested executable steps before rejecting trusted config")
	}
}

func setupRecoveredConfigHistory(t *testing.T, trustedConfig, pushedConfig string) string {
	t.Helper()
	workDir := t.TempDir()
	gitCmd(t, workDir, "init", "--initial-branch=main")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	gitCmd(t, workDir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte(trustedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".no-mistakes.yaml")
	gitCmd(t, workDir, "commit", "-m", "trusted config")
	trustedSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte(pushedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".no-mistakes.yaml")
	gitCmd(t, workDir, "commit", "-m", "pushed config")
	gitCmd(t, workDir, "update-ref", "refs/remotes/origin/main", trustedSHA)
	return workDir
}

func TestLoadRecoveredConfigPreservesTrustedPolicyAndCommandConsent(t *testing.T) {
	oldFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(context.Context, string, string, string) error { return nil }
	t.Cleanup(func() { fetchRecoveredRemoteBranch = oldFetch })

	const pushedConfig = `allow_repo_commands: true
commands:
  lint: echo pushed
routes:
  initial_review: review_strong
document:
  instructions: pushed placement policy
`
	for _, tc := range []struct {
		name               string
		allowPushedCommand bool
		wantCommand        string
	}{
		{name: "trusted commands", wantCommand: "echo trusted"},
		{name: "trusted consent allows pushed commands", allowPushedCommand: true, wantCommand: "echo pushed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow := "false"
			if tc.allowPushedCommand {
				allow = "true"
			}
			trustedConfig := strings.Replace(`allow_repo_commands: ALLOW
commands:
  lint: echo trusted
routes:
  initial_review: authority_strong
document:
  instructions: trusted placement policy
`, "ALLOW", allow, 1)
			workDir := setupRecoveredConfigHistory(t, trustedConfig, pushedConfig)
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			mgr := NewRunManager(nil, p, nil)
			cfg, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "recover-policy"}, &db.Repo{DefaultBranch: "main"}, workDir)
			if err != nil {
				t.Fatalf("load recovered config: %v", err)
			}
			if cfg.Commands.Lint != tc.wantCommand {
				t.Fatalf("commands.lint = %q, want %q", cfg.Commands.Lint, tc.wantCommand)
			}
			route := cfg.Routing.Routes[types.PurposeInitialReview]
			if len(route) != 1 || route[0] != config.ProfileAuthorityStrong {
				t.Fatalf("initial-review route = %v, want trusted authority_strong", route)
			}
			if cfg.Document.Instructions != "trusted placement policy" {
				t.Fatalf("document instructions = %q, want trusted policy", cfg.Document.Instructions)
			}
		})
	}
}

func TestLoadRecoveredConfigRejectsMalformedTrustedPolicy(t *testing.T) {
	oldFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(context.Context, string, string, string) error { return nil }
	t.Cleanup(func() { fetchRecoveredRemoteBranch = oldFetch })

	workDir := setupRecoveredConfigHistory(t,
		"routs:\n  initial_review: authority_strong\n",
		"commands:\n  lint: echo pushed\n",
	)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(nil, p, nil)
	if _, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "recover-invalid"}, &db.Repo{DefaultBranch: "main"}, workDir); err == nil {
		t.Fatal("loadRecoveredConfig accepted malformed trusted policy")
	} else if !strings.Contains(err.Error(), "load trusted repo config") || !strings.Contains(err.Error(), "routs") {
		t.Fatalf("loadRecoveredConfig error = %v, want trusted-config parse error", err)
	}
}

func TestLoadTrustedRepoConfigDistinguishesMissingPolicyFromReadFailure(t *testing.T) {
	workDir := t.TempDir()
	gitCmd(t, workDir, "init", "--initial-branch=main")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	gitCmd(t, workDir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("no policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "README.md")
	gitCmd(t, workDir, "commit", "-m", "no config")
	sha := gitOutput(t, workDir, "rev-parse", "HEAD")

	trusted, err := loadTrustedRepoConfig(context.Background(), workDir, sha, "recover-missing")
	if err != nil {
		t.Fatalf("missing trusted config returned error: %v", err)
	}
	if trusted != nil {
		t.Fatalf("missing trusted config = %+v, want nil", trusted)
	}

	if _, err := loadTrustedRepoConfig(context.Background(), workDir, "not-a-commit", "recover-unreadable"); err == nil {
		t.Fatal("unreadable trusted commit was treated as a missing policy")
	} else if !strings.Contains(err.Error(), "inspect trusted repo config") {
		t.Fatalf("unreadable trusted commit error = %v, want inspection failure", err)
	}
}
