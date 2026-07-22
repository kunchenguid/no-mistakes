package gate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestMain(m *testing.M) {
	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Exit(m.Run())
}

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

// copyDirTree recursively copies src into dst (creating dst). It is a portable
// stand-in for `cp -R`, which is unavailable on Windows.
func copyDirTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy working dir: %v", err)
	}
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

	repo, _, err := Init(ctx, d, p, workDir)
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
	if out, err := exec.Command("git", "-C", bareDir, "config", "--get", "receive.advertisePushOptions").Output(); err != nil {
		t.Fatalf("get receive.advertisePushOptions: %v", err)
	} else if got := string(out); got != "true\n" {
		t.Fatalf("receive.advertisePushOptions = %q, want true", got)
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

func TestInitUnderSafeBareRepositoryExplicit(t *testing.T) {
	// Agent harnesses (e.g. Claude Code) and hardened CI inject this git
	// config, which forbids cwd-based discovery of bare repos, so every git
	// operation on the gate must name it explicitly (issue #362).
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")

	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init under safe.bareRepository=explicit: %v", err)
	}
	if !created {
		t.Error("expected a new gate to be created")
	}

	// Read the gate config naming the repo explicitly; -C discovery would
	// itself fail under the injected setting.
	bareDir := p.RepoDir(repo.ID)
	if out, err := exec.Command("git", "--git-dir="+bareDir, "config", "--get", "receive.advertisePushOptions").Output(); err != nil {
		t.Fatalf("get receive.advertisePushOptions: %v", err)
	} else if got := string(out); got != "true\n" {
		t.Fatalf("receive.advertisePushOptions = %q, want true", got)
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

// TestInitIsIdempotent verifies that re-running Init on an already-initialized
// repo succeeds, reports that it was not newly created, and leaves a single
// repo record and an intact gate. This is what lets existing users re-run init
// to adopt new capabilities (e.g. the agent skill) without hitting an error.
func TestInitIsIdempotent(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	if !created {
		t.Error("first init should report created=true")
	}

	second, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("re-init should succeed, got: %v", err)
	}
	if created {
		t.Error("re-init should report created=false")
	}
	if second.ID != first.ID {
		t.Errorf("re-init repo ID = %q, want %q", second.ID, first.ID)
	}

	// The gate must remain healthy and the DB must not gain a duplicate record.
	bareDir := p.RepoDir(first.ID)
	if !fileExists(filepath.Join(bareDir, "hooks", "post-receive")) {
		t.Error("post-receive hook missing after re-init")
	}
	if url, err := gitpkg.GetRemoteURL(ctx, workDir, RemoteName); err != nil {
		t.Errorf("no-mistakes remote missing after re-init: %v", err)
	} else if url != bareDir {
		t.Errorf("remote url = %q, want %q", url, bareDir)
	}
	dbRepo, err := d.GetRepoByPath(workDir)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if dbRepo == nil || dbRepo.ID != first.ID {
		t.Errorf("expected single repo record %q, got %+v", first.ID, dbRepo)
	}
}

func TestInitWithForkPreservesForkOnPlainReinit(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	parentURL := "https://github.com/parent/project.git"
	forkURL := "https://github.com/fork/project.git"
	localParent, err := gitpkg.GetRemoteURL(ctx, workDir, "origin")
	if err != nil {
		t.Fatalf("get local origin: %v", err)
	}
	localFork := filepath.Join(resolveSymlinks(t, t.TempDir()), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", localFork).CombinedOutput(); err != nil {
		t.Fatalf("init local fork: %v: %s", err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	configureGitInsteadOf(t, workDir, parentURL, localParent)
	configureGitInsteadOf(t, workDir, forkURL, localFork)
	if out, err := exec.Command("git", "-C", workDir, "remote", "set-url", "origin", parentURL).CombinedOutput(); err != nil {
		t.Fatalf("set origin url: %v: %s", err, out)
	}

	first, created, err := InitWithFork(ctx, d, p, workDir, forkURL)
	if err != nil {
		t.Fatalf("first init with fork: %v", err)
	}
	if !created {
		t.Fatal("first init should report created=true")
	}
	if first.ForkURL != forkURL {
		t.Fatalf("fork URL after first init = %q, want %q", first.ForkURL, forkURL)
	}

	second, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("plain re-init: %v", err)
	}
	if created {
		t.Fatal("plain re-init should report created=false")
	}
	if second.ForkURL != forkURL {
		t.Fatalf("fork URL after plain re-init = %q, want preserved %q", second.ForkURL, forkURL)
	}
	dbRepo, err := d.GetRepoByPath(workDir)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if dbRepo == nil || dbRepo.ForkURL != forkURL {
		t.Fatalf("db fork URL after plain re-init = %+v, want %q", dbRepo, forkURL)
	}
}

func TestInitRefreshUpdatesRepoMetadata(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	if !created {
		t.Fatal("first init should report created=true")
	}

	newUpstream := filepath.Join(resolveSymlinks(t, t.TempDir()), "new-upstream.git")
	if out, err := exec.Command("git", "init", "--bare", newUpstream).CombinedOutput(); err != nil {
		t.Fatalf("init new upstream: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workDir, "push", newUpstream, "HEAD:refs/heads/trunk").CombinedOutput(); err != nil {
		t.Fatalf("push trunk: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", newUpstream, "symbolic-ref", "HEAD", "refs/heads/trunk").CombinedOutput(); err != nil {
		t.Fatalf("set new upstream HEAD: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workDir, "remote", "set-url", "origin", newUpstream).CombinedOutput(); err != nil {
		t.Fatalf("set origin url: %v: %s", err, out)
	}

	refreshed, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("refresh init: %v", err)
	}
	if created {
		t.Fatal("refresh init should report created=false")
	}
	if refreshed.ID != first.ID {
		t.Fatalf("refreshed repo ID = %q, want %q", refreshed.ID, first.ID)
	}
	if refreshed.UpstreamURL != newUpstream {
		t.Errorf("refreshed upstream URL = %q, want %q", refreshed.UpstreamURL, newUpstream)
	}
	if refreshed.DefaultBranch != "trunk" {
		t.Errorf("refreshed default branch = %q, want trunk", refreshed.DefaultBranch)
	}

	dbRepo, err := d.GetRepoByPath(workDir)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if dbRepo == nil {
		t.Fatal("expected repo in DB")
	}
	if dbRepo.UpstreamURL != newUpstream {
		t.Errorf("db upstream URL = %q, want %q", dbRepo.UpstreamURL, newUpstream)
	}
	if dbRepo.DefaultBranch != "trunk" {
		t.Errorf("db default branch = %q, want trunk", dbRepo.DefaultBranch)
	}

	gateOrigin, err := gitpkg.GetRemoteURL(ctx, p.RepoDir(first.ID), "origin")
	if err != nil {
		t.Fatalf("get gate origin: %v", err)
	}
	if gateOrigin != newUpstream {
		t.Errorf("gate origin = %q, want %q", gateOrigin, newUpstream)
	}
}

func configureGitInsteadOf(t *testing.T, workDir, rawURL, target string) {
	t.Helper()
	key := fmt.Sprintf("url.%s.insteadOf", gateFileURL(t, target))
	if out, err := exec.Command("git", "-C", workDir, "config", "--global", key, rawURL).CombinedOutput(); err != nil {
		t.Fatalf("configure git insteadOf %s: %v: %s", rawURL, err, out)
	}
}

func gateFileURL(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

func TestInitRefreshUsesPersistedRepoID(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	legacyID := "legacy-repo"
	originURL, err := gitpkg.GetRemoteURL(ctx, workDir, "origin")
	if err != nil {
		t.Fatalf("get origin url: %v", err)
	}
	if _, err := d.InsertRepoWithID(legacyID, workDir, originURL, "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	repo, created, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("refresh init: %v", err)
	}
	if created {
		t.Fatal("refresh init should report created=false")
	}
	if repo.ID != legacyID {
		t.Fatalf("repo ID = %q, want %q", repo.ID, legacyID)
	}

	legacyBareDir := p.RepoDir(legacyID)
	url, err := gitpkg.GetRemoteURL(ctx, workDir, RemoteName)
	if err != nil {
		t.Fatalf("get no-mistakes remote: %v", err)
	}
	if url != legacyBareDir {
		t.Errorf("no-mistakes remote = %q, want %q", url, legacyBareDir)
	}
	if out, err := exec.Command("git", "-C", legacyBareDir, "rev-parse", "--is-bare-repository").Output(); err != nil {
		t.Errorf("legacy bare repo check failed: %v", err)
	} else if got := string(out); got != "true\n" {
		t.Errorf("legacy is-bare = %q, want true", got)
	}
	computedBareDir := p.RepoDir(repoID(workDir))
	if computedBareDir != legacyBareDir && fileExists(computedBareDir) {
		t.Errorf("unexpected computed bare repo exists at %q", computedBareDir)
	}
}

// TestInitRepairsBrokenGate verifies that re-running Init restores gate wiring
// that was torn down out from under it (e.g. a deleted hook or remote), so init
// doubles as a repair command.
func TestInitRepairsBrokenGate(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	bareDir := p.RepoDir(repo.ID)

	// Break the gate: drop the working repo's remote and delete the hook.
	if err := gitpkg.RemoveRemote(ctx, workDir, RemoteName); err != nil {
		t.Fatalf("remove remote: %v", err)
	}
	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	if err := os.Remove(hookPath); err != nil {
		t.Fatalf("remove hook: %v", err)
	}

	if _, _, err := Init(ctx, d, p, workDir); err != nil {
		t.Fatalf("re-init (repair) should succeed: %v", err)
	}

	if !fileExists(hookPath) {
		t.Error("post-receive hook not restored after repair re-init")
	}
	if url, err := gitpkg.GetRemoteURL(ctx, workDir, RemoteName); err != nil {
		t.Errorf("no-mistakes remote not restored after repair re-init: %v", err)
	} else if url != bareDir {
		t.Errorf("restored remote url = %q, want %q", url, bareDir)
	}
}

// TestInitReattachesGateAfterWorkingDirRename verifies that renaming or moving
// a working directory does not break init idempotency: the leftover no-mistakes
// remote identifies the existing gate, and re-running init from the new
// location reattaches it (same repo ID, same bare repo, run history intact)
// instead of failing with "remote already exists".
func TestInitReattachesGateAfterWorkingDirRename(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, err := d.InsertRun(first.ID, "feature", "headsha", "basesha"); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	renamed := filepath.Join(filepath.Dir(workDir), "renamed")
	if err := os.Rename(workDir, renamed); err != nil {
		t.Fatalf("rename working dir: %v", err)
	}

	second, created, err := Init(ctx, d, p, renamed)
	if err != nil {
		t.Fatalf("init after rename should reattach the gate, got: %v", err)
	}
	if created {
		t.Error("init after rename should report created=false")
	}
	if second.ID != first.ID {
		t.Errorf("repo ID after rename = %q, want %q", second.ID, first.ID)
	}
	if second.WorkingPath != renamed {
		t.Errorf("working path = %q, want %q", second.WorkingPath, renamed)
	}

	// The remote must still point at the original gate.
	bareDir := p.RepoDir(first.ID)
	if url, err := gitpkg.GetRemoteURL(ctx, renamed, RemoteName); err != nil {
		t.Errorf("no-mistakes remote missing after reattach: %v", err)
	} else if url != bareDir {
		t.Errorf("remote url = %q, want %q", url, bareDir)
	}

	// The DB record must have migrated to the new path without duplicates.
	dbRepo, err := d.GetRepoByPath(renamed)
	if err != nil {
		t.Fatalf("get repo by new path: %v", err)
	}
	if dbRepo == nil || dbRepo.ID != first.ID {
		t.Fatalf("expected repo %q at new path, got %+v", first.ID, dbRepo)
	}
	if stale, err := d.GetRepoByPath(workDir); err != nil {
		t.Fatalf("get repo by old path: %v", err)
	} else if stale != nil {
		t.Errorf("stale repo record remains at old path: %+v", stale)
	}

	// Run history must survive the reattach.
	runs, err := d.GetRunsByRepo(first.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("runs after reattach = %d, want 1", len(runs))
	}
}

// TestInitCreatesFreshGateForCopiedWorkingDir verifies that when a working
// directory is copied (the original still exists), init treats the copy as a
// new repo: it gets its own gate and the copied no-mistakes remote is
// repointed, while the original repo's gate is left untouched.
func TestInitCreatesFreshGateForCopiedWorkingDir(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}

	copyDir := filepath.Join(filepath.Dir(workDir), "copy")
	copyDirTree(t, workDir, copyDir)

	second, created, err := Init(ctx, d, p, copyDir)
	if err != nil {
		t.Fatalf("init on copy should succeed, got: %v", err)
	}
	if !created {
		t.Error("init on copy should report created=true")
	}
	if second.ID == first.ID {
		t.Error("copy should get its own gate, not reuse the original's")
	}

	// The copy's remote must point at its own gate.
	if url, err := gitpkg.GetRemoteURL(ctx, copyDir, RemoteName); err != nil {
		t.Errorf("no-mistakes remote missing on copy: %v", err)
	} else if url != p.RepoDir(second.ID) {
		t.Errorf("copy remote url = %q, want %q", url, p.RepoDir(second.ID))
	}

	// The original must be untouched.
	if url, err := gitpkg.GetRemoteURL(ctx, workDir, RemoteName); err != nil {
		t.Errorf("original no-mistakes remote missing: %v", err)
	} else if url != p.RepoDir(first.ID) {
		t.Errorf("original remote url = %q, want %q", url, p.RepoDir(first.ID))
	}
	if dbRepo, err := d.GetRepoByPath(workDir); err != nil || dbRepo == nil || dbRepo.ID != first.ID {
		t.Errorf("original repo record damaged: %+v, %v", dbRepo, err)
	}
}

// TestInitRepointsOrphanGateRemoteOnFreshInit verifies that a leftover
// no-mistakes remote pointing into our repos dir with no matching DB record
// (a half-ejected gate) is repointed to the fresh gate instead of failing.
func TestInitRepointsOrphanGateRemoteOnFreshInit(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	// Orphan the gate: drop the DB record, then move the working dir so the
	// computed gate path no longer matches the leftover remote.
	if err := d.DeleteRepo(first.ID); err != nil {
		t.Fatalf("delete repo record: %v", err)
	}
	renamed := filepath.Join(filepath.Dir(workDir), "renamed")
	if err := os.Rename(workDir, renamed); err != nil {
		t.Fatalf("rename working dir: %v", err)
	}

	repo, created, err := Init(ctx, d, p, renamed)
	if err != nil {
		t.Fatalf("init with orphan remote should succeed, got: %v", err)
	}
	if !created {
		t.Error("init with orphan remote should report created=true")
	}
	if url, err := gitpkg.GetRemoteURL(ctx, renamed, RemoteName); err != nil {
		t.Errorf("no-mistakes remote missing: %v", err)
	} else if url != p.RepoDir(repo.ID) {
		t.Errorf("remote url = %q, want %q", url, p.RepoDir(repo.ID))
	}
}

func TestInitDoesNotOverwriteExistingNoMistakesRemoteOnFreshInit(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	customRemote := filepath.Join(resolveSymlinks(t, t.TempDir()), "custom.git")
	if out, err := exec.Command("git", "init", "--bare", customRemote).CombinedOutput(); err != nil {
		t.Fatalf("init custom remote: %v: %s", err, out)
	}
	if err := gitpkg.AddRemote(ctx, workDir, RemoteName, customRemote); err != nil {
		t.Fatalf("add custom remote: %v", err)
	}

	_, _, err := Init(ctx, d, p, workDir)
	if err == nil {
		t.Fatal("expected init to fail when no-mistakes remote already exists")
	}

	url, err := gitpkg.GetRemoteURL(ctx, workDir, RemoteName)
	if err != nil {
		t.Fatalf("get custom remote: %v", err)
	}
	if url != customRemote {
		t.Errorf("no-mistakes remote = %q, want %q", url, customRemote)
	}
}

func TestInitRefreshPreservesCustomPostReceiveHook(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	hookPath := filepath.Join(p.RepoDir(repo.ID), "hooks", "post-receive")
	customHook := []byte("#!/bin/sh\necho custom hook\n")
	if err := os.WriteFile(hookPath, customHook, 0o755); err != nil {
		t.Fatalf("write custom hook: %v", err)
	}

	if _, _, err := Init(ctx, d, p, workDir); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if string(got) != string(customHook) {
		t.Errorf("custom hook was overwritten")
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

	_, _, err := Init(context.Background(), d, p, work)
	if err == nil {
		t.Fatal("expected error when no origin remote")
	}
	// The error must be actionable, not a raw git-plumbing leak: a fresh
	// `git init` repo with no remote yet is a normal state, so tell the user
	// how to fix it instead of surfacing `git remote get-url` exit codes.
	msg := err.Error()
	if !strings.Contains(msg, "git remote add origin") {
		t.Errorf("error should tell the user how to add an origin remote; got: %q", msg)
	}
	if strings.Contains(msg, "get origin url") || strings.Contains(msg, "exit status") {
		t.Errorf("error leaked raw git plumbing; got: %q", msg)
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

	_, _, err := Init(context.Background(), d, p, notGit)
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

	repo, _, err := Init(context.Background(), d, p, work)
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

	repo, _, err := Init(ctx, d, p, workDir)
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

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Create a fake worktree directory to verify cleanup.
	wtDir := p.WorktreeDir(repo.ID, "fake-run-id")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
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

// TestInit_PostReceiveSurvivesHooksPathPoisoning reproduces issue #122.
// Husky and similar tools run `git config core.hookspath .husky/_` from
// inside a worktree of the gate bare repo. That write lands in the bare's
// shared local config and silently disables the post-receive hook, so
// subsequent pushes complete but never trigger a pipeline.
//
// The gate repo must isolate its own core.hookspath so external writes to
// the shared config can't reach it.
func TestInit_PostReceiveSurvivesHooksPathPoisoning(t *testing.T) {
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	bareDir := p.RepoDir(repo.ID)

	// Replace the installed hook with one that touches a marker file so we
	// can detect whether receive-pack actually invokes hooks/post-receive.
	markerDir := resolveSymlinks(t, t.TempDir())
	marker := filepath.Join(markerDir, "fired")
	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	hook := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatalf("write marker hook: %v", err)
	}

	// Simulate husky: pnpm install in a pipeline worktree runs
	// `git config core.hookspath .husky/_`. Because worktrees share local
	// config with the bare main repo, that write lands in bareDir/config.
	if out, err := exec.Command("git", "-C", bareDir, "config", "core.hookspath", ".husky/_").CombinedOutput(); err != nil {
		t.Fatalf("simulate husky poisoning: %v: %s", err, out)
	}

	// Push to the gate. The bare repo's own core.hookspath must still
	// resolve to its hooks dir so post-receive fires.
	if out, err := exec.Command("git", "-C", workDir, "push", "no-mistakes", "HEAD:refs/heads/test-branch").CombinedOutput(); err != nil {
		t.Fatalf("push: %v: %s", err, out)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post-receive did not fire after husky poisoned core.hookspath: %v", err)
	}
}

// TestInitRedactsCredentialURL asserts that an origin URL carrying embedded
// credentials (e.g. a fine-grained PAT) is stored redacted in the DB and in the
// returned repo record, while the bare gate's "origin" remote keeps the full
// credentialled URL so worktrees carved from it can still authenticate pushes.
//
// The upstream host is an unreachable loopback address so DefaultBranch's
// ls-remote fails fast and falls back to "main" without real network I/O.
func TestInitRedactsCredentialURL(t *testing.T) {
	workDir := setupTestRepo(t)
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@127.0.0.1:1/o/r.git"
	if out, err := exec.Command("git", "-C", workDir, "remote", "set-url", "origin", credURL).CombinedOutput(); err != nil {
		t.Fatalf("set credentialled origin: %v: %s", err, out)
	}

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// The returned repo record (and thus anything logged from it) must not
	// carry the credential.
	if strings.Contains(repo.UpstreamURL, token) {
		t.Errorf("returned repo.UpstreamURL leaked credential: %q", repo.UpstreamURL)
	}
	if !strings.Contains(repo.UpstreamURL, "redacted@127.0.0.1:1/o/r.git") {
		t.Errorf("expected redacted URL, got %q", repo.UpstreamURL)
	}

	// The persisted DB row must match the redacted form.
	dbRepo, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if strings.Contains(dbRepo.UpstreamURL, token) {
		t.Errorf("DB repo.UpstreamURL leaked credential: %q", dbRepo.UpstreamURL)
	}

	// The bare gate's origin must still carry the full credential so pushes
	// from carved worktrees can authenticate.
	bareDir := p.RepoDir(repo.ID)
	gateOrigin, err := gitpkg.GetRemoteURL(ctx, bareDir, "origin")
	if err != nil {
		t.Fatalf("get gate origin: %v", err)
	}
	if gateOrigin != credURL {
		t.Errorf("gate origin = %q, want full credentialled URL %q (credential must be preserved for pushes)", gateOrigin, credURL)
	}
}
