//go:build e2e

package e2e

import (
	"context"
	"os"
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

	h := NewHarness(t, SetupOpts{Agent: "claude"})
	badNMHome := filepath.Join(t.TempDir(), strings.Repeat("a", 160))
	env := map[string]string{
		"NM_HOME":                            badNMHome,
		"NM_TEST_DAEMON_START_TIMEOUT":       "200ms",
		"NM_TEST_DAEMON_START_POLL_INTERVAL": "10ms",
	}

	start := time.Now()
	out, err := h.RunInDirWithEnv(h.WorkDir, env, "init")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("init should fail when daemon startup fails")
	}
	if !strings.Contains(out, "start daemon") {
		t.Fatalf("init output = %q, want daemon startup failure", out)
	}
	if strings.Contains(out, "rollback init:") {
		t.Fatalf("rollback should succeed cleanly, got wrapped error output: %q", out)
	}
	if elapsed >= time.Second {
		t.Fatalf("init rollback should fail fast in tests, took %v", elapsed)
	}

	ctx := context.Background()
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "get-url", "no-mistakes"); err == nil {
		t.Fatalf("no-mistakes remote should be removed after failed init, got %q", out)
	}

	p := paths.WithRoot(badNMHome)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	gitRoot, err := git.FindGitRoot(h.WorkDir)
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
