package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
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

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Insert a run directly into the DB to simulate data.
	p, d, err := openResources()
	if err != nil {
		t.Fatal(err)
	}
	_ = p

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
	d.Close()

	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs failed: %v\noutput: %s", err, out)
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

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Insert many runs.
	_, d, err := openResources()
	if err != nil {
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

	for i := 0; i < 15; i++ {
		if _, err := d.InsertRun(repo.ID, "branch", "sha", "base"); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

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

func TestRunsFromWorktree(t *testing.T) {
	// Init main repo, create a worktree, chdir into it, and verify that
	// `runs` (via findRepo) finds the repo through the worktree fallback.
	repoDir := setupTestRepo(t)

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Create a branch and a worktree.
	run(t, repoDir, "git", "checkout", "-b", "wt-branch")
	run(t, repoDir, "git", "checkout", "-")
	wtDir := filepath.Join(t.TempDir(), "worktree")
	ctx := context.Background()
	if err := git.WorktreeAdd(ctx, repoDir, wtDir, "wt-branch"); err != nil {
		t.Fatalf("WorktreeAdd failed: %v", err)
	}
	t.Cleanup(func() { git.WorktreeRemove(ctx, repoDir, wtDir) })

	// Move into the worktree directory.
	chdir(t, wtDir)

	// `runs` should succeed because findRepo falls back to the main repo root.
	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "no runs") {
		t.Errorf("runs output should say 'no runs', got: %s", out)
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
