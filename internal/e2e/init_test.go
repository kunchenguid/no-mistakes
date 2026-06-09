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

// TestInitIsIdempotent proves an existing user can re-run `init` to adopt new
// capabilities (the agent skill) without hitting an "already initialized"
// error, and that the second run reports the refresh and re-installs the skill.
func TestInitIsIdempotent(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	first, err := h.RunInDir(h.WorkDir, "init")
	if err != nil {
		t.Fatalf("first init: %v\n%s", err, first)
	}
	if !strings.Contains(first, "Gate initialized") {
		t.Errorf("first init should report a fresh gate, got:\n%s", first)
	}
	assertSkillInstalled(t, h)

	// Remove the installed skill to prove the re-run reinstalls it.
	skillPath := filepath.Join(h.WorkDir, ".claude", "skills", "no-mistakes", "SKILL.md")
	if err := os.Remove(skillPath); err != nil {
		t.Fatalf("remove skill: %v", err)
	}

	second, err := h.RunInDir(h.WorkDir, "init")
	if err != nil {
		t.Fatalf("re-init should succeed: %v\n%s", err, second)
	}
	if !strings.Contains(second, "already initialized") {
		t.Errorf("re-init should report an existing gate, got:\n%s", second)
	}
	if strings.Contains(second, "already initialized for") {
		t.Errorf("re-init must not fail with the old error, got:\n%s", second)
	}
	assertSkillInstalled(t, h)

	// The no-mistakes remote must still be wired after the refresh.
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "get-url", "no-mistakes"); err != nil {
		t.Fatalf("no-mistakes remote missing after re-init: %v\n%s", err, out)
	}
}

// TestInitRepoRename proves that a user who renames or moves their repo
// directory can re-run `init` from the new location and get their existing
// gate back, instead of the historical failure:
//
//	init: add remote: remote "no-mistakes" already exists with url "..."
//
// The test name is deliberately short: it becomes part of t.TempDir(), which
// hosts NM_HOME, and the daemon's Unix socket path under it must stay within
// the OS socket path limit (104 bytes on macOS).
func TestInitRepoRename(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	first, err := h.RunInDir(h.WorkDir, "init")
	if err != nil {
		t.Fatalf("first init: %v\n%s", err, first)
	}
	gateURLOut, err := h.runGit(ctx, h.WorkDir, "remote", "get-url", "no-mistakes")
	if err != nil {
		t.Fatalf("get gate url: %v\n%s", err, gateURLOut)
	}
	gateURL := strings.TrimSpace(string(gateURLOut))

	renamed := filepath.Join(filepath.Dir(h.WorkDir), "work-renamed")
	if err := os.Rename(h.WorkDir, renamed); err != nil {
		t.Fatalf("rename work dir: %v", err)
	}

	second, err := h.RunInDir(renamed, "init")
	if err != nil {
		t.Fatalf("init after rename should succeed: %v\n%s", err, second)
	}
	if !strings.Contains(second, "already initialized") {
		t.Errorf("init after rename should report an existing gate, got:\n%s", second)
	}

	// The repo must be reattached to the same gate, not a new one.
	if out, err := h.runGit(ctx, renamed, "remote", "get-url", "no-mistakes"); err != nil {
		t.Fatalf("no-mistakes remote missing after rename re-init: %v\n%s", err, out)
	} else if url := strings.TrimSpace(string(out)); url != gateURL {
		t.Errorf("gate url after rename = %q, want %q", url, gateURL)
	}
}

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
