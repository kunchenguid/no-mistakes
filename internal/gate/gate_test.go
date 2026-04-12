package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// resolveSymlinks resolves symlinks in a path (needed on macOS where
// /var → /private/var but git returns resolved paths).
func resolveSymlinks(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("eval symlinks %q: %v", p, err)
	}
	return resolved
}

// setupTestRepo creates a git repo with an origin remote and returns its resolved path.
func setupTestRepo(t *testing.T) string {
	t.Helper()

	// Create an "upstream" bare repo to act as origin.
	upstream := filepath.Join(resolveSymlinks(t, t.TempDir()), "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", upstream).CombinedOutput(); err != nil {
		t.Fatalf("init upstream: %v: %s", err, out)
	}

	// Create working repo and add origin.
	work := filepath.Join(resolveSymlinks(t, t.TempDir()), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.email", "test@test.com").CombinedOutput(); err != nil {
		t.Fatalf("config email: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.name", "Test").CombinedOutput(); err != nil {
		t.Fatalf("config name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", upstream).CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v: %s", err, out)
	}

	// Make an initial commit so HEAD exists.
	if out, err := exec.Command("git", "-C", work, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("initial commit: %v: %s", err, out)
	}

	return work
}

func openTestDB(t *testing.T, p *paths.Paths) *db.DB {
	t.Helper()
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestInit(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Verify repo record was created with correct fields.
	if repo.ID == "" {
		t.Error("expected non-empty repo ID")
	}
	if repo.WorkingPath != workDir {
		t.Errorf("working path = %q, want %q", repo.WorkingPath, workDir)
	}
	if repo.UpstreamURL == "" {
		t.Error("expected non-empty upstream URL")
	}

	// Verify bare repo was created.
	bareDir := p.RepoDir(repo.ID)
	if out, err := exec.Command("git", "-C", bareDir, "rev-parse", "--is-bare-repository").Output(); err != nil {
		t.Errorf("bare repo check failed: %v", err)
	} else if got := string(out); got != "true\n" {
		t.Errorf("is-bare = %q, want true", got)
	}

	// Verify post-receive hook was installed.
	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	if !fileExists(hookPath) {
		t.Error("post-receive hook not installed")
	}

	// Verify no-mistakes remote was added to working repo.
	url, err := gitpkg.GetRemoteURL(ctx, workDir, "no-mistakes")
	if err != nil {
		t.Fatalf("get remote url: %v", err)
	}
	if url != bareDir {
		t.Errorf("remote url = %q, want %q", url, bareDir)
	}

	// Verify the gate bare repo knows the upstream as origin so gh can resolve repo context.
	originURL, err := gitpkg.GetRemoteURL(ctx, bareDir, "origin")
	if err != nil {
		t.Fatalf("get gate origin url: %v", err)
	}
	if originURL != repo.UpstreamURL {
		t.Errorf("gate origin url = %q, want %q", originURL, repo.UpstreamURL)
	}

	// Verify repo record exists in DB.
	dbRepo, err := d.GetRepoByPath(workDir)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if dbRepo == nil {
		t.Fatal("expected repo in DB")
	}
	if dbRepo.ID != repo.ID {
		t.Errorf("db repo id = %q, want %q", dbRepo.ID, repo.ID)
	}
}

func TestInitRepoID(t *testing.T) {
	// Verify repo ID is deterministic based on path.
	id1 := repoID("/some/path")
	id2 := repoID("/some/path")
	if id1 != id2 {
		t.Errorf("repo IDs should be deterministic: %q != %q", id1, id2)
	}
	if len(id1) != 12 {
		t.Errorf("repo ID length = %d, want 12", len(id1))
	}

	// Different paths produce different IDs.
	id3 := repoID("/other/path")
	if id1 == id3 {
		t.Error("different paths should produce different IDs")
	}
}

func TestInitAlreadyInitialized(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	if _, err := Init(ctx, d, p, workDir); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// Second init should fail.
	_, err := Init(ctx, d, p, workDir)
	if err == nil {
		t.Fatal("expected error on re-init")
	}
}

func TestInitNoOrigin(t *testing.T) {
	// Create a repo without origin.
	work := filepath.Join(resolveSymlinks(t, t.TempDir()), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	_, err := Init(context.Background(), d, p, work)
	if err == nil {
		t.Fatal("expected error when no origin remote")
	}
}

func TestInitNotGitRepo(t *testing.T) {
	notGit := t.TempDir()
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	_, err := Init(context.Background(), d, p, notGit)
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func TestInitDetectsDefaultBranchFromRemote(t *testing.T) {
	// Create upstream with "develop" as default branch.
	upstream := filepath.Join(resolveSymlinks(t, t.TempDir()), "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", upstream).CombinedOutput(); err != nil {
		t.Fatalf("init upstream: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", upstream, "symbolic-ref", "HEAD", "refs/heads/develop").CombinedOutput(); err != nil {
		t.Fatalf("set HEAD: %v: %s", err, out)
	}

	// Create working repo, push develop branch, then checkout a feature branch.
	work := filepath.Join(resolveSymlinks(t, t.TempDir()), "work")
	if out, err := exec.Command("git", "init", "-b", "develop", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.email", "test@test.com").CombinedOutput(); err != nil {
		t.Fatalf("config email: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.name", "Test").CombinedOutput(); err != nil {
		t.Fatalf("config name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", upstream).CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "push", "origin", "develop").CombinedOutput(); err != nil {
		t.Fatalf("push: %v: %s", err, out)
	}
	// Switch to a feature branch — Init should NOT use this as default_branch.
	if out, err := exec.Command("git", "-C", work, "checkout", "-b", "feature/my-work").CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v: %s", err, out)
	}

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	repo, err := Init(context.Background(), d, p, work)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Default branch should be "develop" (from upstream HEAD), not "feature/my-work".
	if repo.DefaultBranch != "develop" {
		t.Errorf("default branch = %q, want 'develop'", repo.DefaultBranch)
	}
}

func TestEject(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	if _, err := Eject(ctx, d, p, workDir); err != nil {
		t.Fatalf("eject: %v", err)
	}

	// Verify remote was removed.
	_, err = gitpkg.GetRemoteURL(ctx, workDir, "no-mistakes")
	if err == nil {
		t.Error("expected no-mistakes remote to be removed")
	}

	// Verify bare repo was deleted.
	bareDir := p.RepoDir(repo.ID)
	if fileExists(bareDir) {
		t.Error("expected bare repo to be deleted")
	}

	// Verify DB record was deleted.
	dbRepo, err := d.GetRepoByPath(workDir)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if dbRepo != nil {
		t.Error("expected repo to be deleted from DB")
	}
}

func TestEjectCleansUpWorktrees(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Create a fake worktree directory to verify cleanup.
	wtDir := p.WorktreeDir(repo.ID, "fake-run-id")
	if err := exec.Command("mkdir", "-p", wtDir).Run(); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}

	if _, err := Eject(ctx, d, p, workDir); err != nil {
		t.Fatalf("eject: %v", err)
	}

	// Verify worktree directory was cleaned up.
	repoWtDir := filepath.Join(p.WorktreesDir(), repo.ID)
	if fileExists(repoWtDir) {
		t.Error("expected worktree directory to be cleaned up")
	}
}

func TestEjectNotInitialized(t *testing.T) {
	work := filepath.Join(resolveSymlinks(t, t.TempDir()), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	_, err := Eject(context.Background(), d, p, work)
	if err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
