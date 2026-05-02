package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestInitRollsBackWhenDaemonStartFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows IPC does not use Unix socket path limits")
	}

	repoDir := setupTestRepo(t)
	nmHome := filepath.Join(t.TempDir(), strings.Repeat("a", 160))
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "200ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	start := time.Now()
	_, err := executeCmd("init")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("init should fail when daemon startup fails")
	}
	if !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("init error = %v, want daemon startup failure", err)
	}
	if strings.Contains(err.Error(), "rollback init:") {
		t.Fatalf("rollback should succeed cleanly, got wrapped error: %v", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("init rollback should fail fast in tests, took %v", elapsed)
	}

	cmd := exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		t.Fatal("no-mistakes remote should be removed after failed init")
	}

	p := paths.WithRoot(nmHome)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}
	if repo != nil {
		t.Fatal("repo record should be removed after failed init")
	}

	entries, err := os.ReadDir(p.ReposDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no bare repos after failed init, found %d", len(entries))
	}
}
