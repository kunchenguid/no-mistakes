//go:build e2e

package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestRunsNotInitialized(t *testing.T) {
	setupTestRepo(t)

	_, err := executeCmd("runs")
	if err == nil {
		t.Fatal("runs should fail when not initialized")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

func TestRunsEmpty(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatalf("gate.Init failed: %v", err)
	}

	out, err := executeCmd("runs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no runs") {
		t.Errorf("runs output should say 'no runs', got: %s", out)
	}
}

func TestRunsWithData(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatalf("gate.Init failed: %v", err)
	}

	// Insert a run directly into the DB to simulate data.
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	run, err := d.InsertRun(repo.ID, "feature-branch", "abc1234", "def5678")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	// Insert a second run that's running.
	run2, err := d.InsertRun(repo.ID, "another-branch", "111aaa", "222bbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run2.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs failed: %v\noutput: %s", err, out)
	}
	if ansiEscapeRE.MatchString(out) {
		t.Fatalf("runs output should not include ANSI escape sequences, got: %q", out)
	}
	if !strings.Contains(out, "another-branch") {
		t.Errorf("runs output should contain 'another-branch', got: %s", out)
	}
	if !strings.Contains(out, "feature-branch") {
		t.Errorf("runs output should contain 'feature-branch', got: %s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("runs output should contain 'running' status, got: %s", out)
	}
	if !strings.Contains(out, "completed") {
		t.Errorf("runs output should contain 'completed' status, got: %s", out)
	}
}

func TestRunsLimit(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatalf("gate.Init failed: %v", err)
	}

	// Insert many runs.
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 15; i++ {
		if _, err := d.InsertRun(repo.ID, "branch", "sha", "base"); err != nil {
			t.Fatal(err)
		}
	}

	// Default limit should show max 10 runs.
	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Count data lines (skip header and empty lines).
	dataLines := 0
	for _, line := range lines {
		if strings.Contains(line, "branch") && strings.Contains(line, "pending") {
			dataLines++
		}
	}
	if dataLines > 10 {
		t.Errorf("default runs output should show at most 10 runs, got %d", dataLines)
	}
}

func TestRunsFromWorktreeWithActiveRun(t *testing.T) {
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

	run(t, repoDir, "git", "checkout", "-b", "wt-runs-branch")
	run(t, repoDir, "git", "checkout", "-")
	wtDir := filepath.Join(t.TempDir(), "worktree")
	ctx := context.Background()
	if err := git.WorktreeAdd(ctx, repoDir, wtDir, "wt-runs-branch"); err != nil {
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

	r, err := d.InsertRun(repo.ID, "wt-runs-branch", "abc123456789", "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	chdir(t, wtDir)

	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "wt-runs-branch") {
		t.Errorf("expected worktree branch in runs output, got: %s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("expected running status in runs output, got: %s", out)
	}
	if !strings.Contains(out, "abc12345") {
		t.Errorf("expected truncated head SHA in runs output, got: %s", out)
	}
	if strings.Contains(out, "no runs") {
		t.Errorf("runs output should show the active run instead of empty-state text, got: %s", out)
	}
}

func TestRunsNotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	chdir(t, tmpDir)

	_, err := executeCmd("runs")
	if err == nil {
		t.Fatal("runs should fail outside a git repo")
	}
}

// Helper to add the db package import for test compilation.
var _ = (*db.DB)(nil)
