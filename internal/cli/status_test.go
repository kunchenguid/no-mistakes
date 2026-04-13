package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStatusNotInitialized(t *testing.T) {
	setupTestRepo(t)

	out, err := executeCmd("status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("status output should say 'not initialized', got: %s", out)
	}
}

func TestStatusInitialized(t *testing.T) {
	repoDir := setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}

	remoteOut, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("resolve origin remote: %v", err)
	}
	upstreamURL := strings.TrimSpace(string(remoteOut))

	out, err := executeCmd("status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, gitRoot) {
		t.Errorf("status output should contain repo path %q, got: %s", gitRoot, out)
	}
	if !strings.Contains(out, upstreamURL) {
		t.Errorf("status output should contain upstream remote %q, got: %s", upstreamURL, out)
	}
	if !strings.Contains(out, "daemon:") || !strings.Contains(out, "stopped") {
		t.Errorf("status output should show stopped daemon, got: %s", out)
	}
	if !strings.Contains(out, "no active run") {
		t.Errorf("status output should show empty active run state, got: %s", out)
	}
	if strings.Contains(out, repoDir) && repoDir != gitRoot {
		t.Logf("status output uses resolved repo path %q instead of temp dir path %q", gitRoot, repoDir)
	}
}

func TestStatusNotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	chdir(t, tmpDir)

	out, err := executeCmd("status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not in a git repository") {
		t.Errorf("status output should say 'not in a git repository', got: %s", out)
	}
}

func TestStatusWithActiveRun(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	// Look up the repo.
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a running run so it shows as active.
	r, err := d.InsertRun(repo.ID, "feature/auth", "abcdef1234567890", "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, "running"); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("status")
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}

	// Should show active run section.
	if !strings.Contains(out, "Active run") {
		t.Errorf("expected 'Active run' section, got: %s", out)
	}
	if !strings.Contains(out, "feature/auth") {
		t.Errorf("expected branch 'feature/auth', got: %s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("expected status 'running', got: %s", out)
	}
	// Head SHA should be truncated to 8 chars.
	if !strings.Contains(out, "abcdef12") {
		t.Errorf("expected truncated head SHA 'abcdef12', got: %s", out)
	}
}

func TestStatusWithShortHeadSHA(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	r, err := d.InsertRun(repo.ID, "feature/short-sha", "abc123", "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, "running"); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("status")
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}

	if !strings.Contains(out, "abc123") {
		t.Errorf("expected full short head SHA 'abc123', got: %s", out)
	}
	if strings.Contains(out, "00000000") {
		t.Errorf("status output should show the active run head SHA, got: %s", out)
	}
}

func TestStatusFromWorktreeWithActiveRun(t *testing.T) {
	repoDir := setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, repoDir); err != nil {
		t.Fatalf("gate.Init failed: %v", err)
	}

	run(t, repoDir, "git", "checkout", "-b", "wt-status-branch")
	run(t, repoDir, "git", "checkout", "-")
	wtDir := filepath.Join(t.TempDir(), "worktree")
	ctx := context.Background()
	if err := git.WorktreeAdd(ctx, repoDir, wtDir, "wt-status-branch"); err != nil {
		t.Fatalf("WorktreeAdd failed: %v", err)
	}
	cleanupWorktree(t, repoDir, wtDir)

	gitRoot, err := git.FindGitRoot(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	r, err := d.InsertRun(repo.ID, "wt-status-branch", "abc123456789", "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, "running"); err != nil {
		t.Fatal(err)
	}

	chdir(t, wtDir)

	out, err := executeCmd("status")
	if err != nil {
		t.Fatalf("status from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Active run") {
		t.Errorf("expected active run section from worktree, got: %s", out)
	}
	if !strings.Contains(out, "wt-status-branch") {
		t.Errorf("expected worktree branch in status output, got: %s", out)
	}
	if !strings.Contains(out, "abc12345") {
		t.Errorf("expected truncated head SHA in status output, got: %s", out)
	}
	if !strings.Contains(out, gitRoot) {
		t.Errorf("expected status output to show initialized repo path %q, got: %s", gitRoot, out)
	}
}
