package cli

import (
	"context"
	"os"
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
	setupTestRepo(t)

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	out, err := executeCmd("status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "repo:") {
		t.Errorf("status output should contain repo info, got: %s", out)
	}
	if !strings.Contains(out, "remote:") {
		t.Errorf("status output should contain remote info, got: %s", out)
	}
	if !strings.Contains(out, "daemon:") {
		t.Errorf("status output should contain daemon status, got: %s", out)
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

func TestMinLen(t *testing.T) {
	if got := minLen(5, 3); got != 3 {
		t.Errorf("minLen(5, 3) = %d, want 3", got)
	}
	if got := minLen(3, 5); got != 3 {
		t.Errorf("minLen(3, 5) = %d, want 3", got)
	}
	if got := minLen(4, 4); got != 4 {
		t.Errorf("minLen(4, 4) = %d, want 4", got)
	}
}
