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
// error, and that the second run reports the refresh and refreshes a stale
// user-level skill copy left by an older binary.
func TestInitIsIdempotent(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	first, err := h.RunInDir(h.WorkDir, "init")
	if err != nil {
		t.Fatalf("first init: %v\n%s", err, first)
	}
	if !strings.Contains(first, "Gate initialized") {
		t.Errorf("first init should report a fresh gate, got:\n%s", first)
	}
	if strings.Contains(first, "no longer needed") {
		t.Errorf("init in a repo without a vendored skill copy must not print the legacy notice, got:\n%s", first)
	}
	assertSkillInstalled(t, h)

	// Overwrite the installed skill with stale content to prove the re-run
	// refreshes the user-level copy.
	skillPath := filepath.Join(h.HomeDir, ".claude", "skills", "no-mistakes", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: no-mistakes\n---\nstale body\n"), 0o644); err != nil {
		t.Fatalf("write stale skill: %v", err)
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
	if data, err := os.ReadFile(skillPath); err != nil {
		t.Fatalf("read skill after re-init: %v", err)
	} else if strings.Contains(string(data), "stale body") {
		t.Errorf("re-init must refresh a stale user-level skill copy")
	}

	// The no-mistakes remote must still be wired after the refresh.
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "get-url", "no-mistakes"); err != nil {
		t.Fatalf("no-mistakes remote missing after re-init: %v\n%s", err, out)
	}
}

// TestInitLegacyNotice proves init in a repo that still carries a vendored
// skill copy from an older no-mistakes version points it out without touching
// it: the copy is the user's to remove, possibly via their VCS.
//
// The test name is deliberately short for the same socket path length reason
// as TestInitRepoRename.
func TestInitLegacyNotice(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	legacy := filepath.Join(h.WorkDir, ".agents", "skills", "no-mistakes", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent := "---\nname: no-mistakes\nmetadata:\n  internal: true\n---\nlegacy vendored body\n"
	if err := os.WriteFile(legacy, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := h.RunInDir(h.WorkDir, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no longer needed") {
		t.Errorf("init should notice the vendored legacy copy, got:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(".agents", "skills", "no-mistakes", "SKILL.md")) {
		t.Errorf("the notice should name the vendored path, got:\n%s", out)
	}

	// The vendored copy must be left exactly as it was.
	data, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("legacy copy must survive init: %v", err)
	}
	if string(data) != legacyContent {
		t.Errorf("init must not modify the vendored copy")
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

// TestInitInSubmoduleRegistersSubmoduleOwnOrigin reproduces issue #328:
// `no-mistakes init` from inside a Git submodule checkout must register the
// gate against the submodule's own origin, not the superproject's. Before the
// fix, FindMainRepoRoot took the parent of --git-common-dir, which for an
// absorbed submodule is <super>/.git/modules/<name>. The gate then read its
// origin from that wrong root and recorded the superproject's URL as the
// upstream — a "push would land in the wrong repository" data-loss hazard.
func TestInitInSubmoduleRegistersSubmoduleOwnOrigin(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	// Second bare repo serving as the submodule's "origin", distinct from
	// the superproject's upstream (h.UpstreamDir) so the assertion below can
	// tell which one the gate recorded.
	subUpstream := filepath.Join(filepath.Dir(h.UpstreamDir), "sub-upstream.git")
	if err := os.MkdirAll(subUpstream, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(ctx, subUpstream, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init sub upstream: %v\n%s", err, out)
	}
	// Seed the submodule's upstream with a real commit so `submodule add`
	// can clone and check out a branch. Without it, the empty-repo clone
	// dies with "fatal: You are on a branch yet to be born".
	subSeed := filepath.Join(t.TempDir(), "sub-seed")
	if err := os.MkdirAll(subSeed, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(ctx, subSeed, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("init sub seed: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(subSeed, "README.md"), []byte("# submodule upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(ctx, subSeed, "add", "."); err != nil {
		t.Fatalf("add sub seed: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, subSeed, "commit", "-m", "initial"); err != nil {
		t.Fatalf("commit sub seed: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, subSeed, "remote", "add", "origin", subUpstream); err != nil {
		t.Fatalf("remote add sub seed: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, subSeed, "push", "-u", "origin", "main"); err != nil {
		t.Fatalf("push sub seed: %v\n%s", err, out)
	}

	// Build the submodule in <work>/sub pointing at subUpstream.
	if out, err := h.runGit(ctx, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "add", subUpstream, "sub"); err != nil {
		t.Fatalf("submodule add: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "init", "sub"); err != nil {
		t.Fatalf("submodule init: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "update", "sub"); err != nil {
		t.Fatalf("submodule update: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", ".gitmodules", "sub"); err != nil {
		t.Fatalf("git add submodule: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "add sub submodule"); err != nil {
		t.Fatalf("commit submodule: %v\n%s", err, out)
	}

	subWorktree := filepath.Join(h.WorkDir, "sub")

	// Run init from the submodule checkout. The pre-fix behavior writes the
	// superproject's upstream into the gate row; the post-fix behavior must
	// write the submodule's own.
	if out, err := h.RunInDir(subWorktree, "init"); err != nil {
		t.Fatalf("init in submodule: %v\n%s", err, out)
	}

	p := paths.WithRoot(h.NMHome)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// Find the gate by its working path (the submodule's own working tree,
	// not the superproject's). The pre-fix code stored the superproject path
	// and never created a row for the submodule, so the by-path lookup
	// returns nil; the post-fix code stores the submodule path and the
	// recorded upstream must match the submodule's origin.
	resolvedSub, _ := filepath.EvalSymlinks(subWorktree)
	repo, err := d.GetRepoByPath(resolvedSub)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if repo == nil {
		t.Fatalf("no gate registered for submodule working tree %q; init must record the submodule's own path, not the superproject's", resolvedSub)
	}
	if repo.UpstreamURL != subUpstream {
		t.Fatalf("gate upstream = %q, want %q (the submodule's own origin); recording the superproject's origin is the data-loss bug from issue #328", repo.UpstreamURL, subUpstream)
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
