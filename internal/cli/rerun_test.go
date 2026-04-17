package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRerunNotInitialized(t *testing.T) {
	setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	_, err = executeCmd("rerun")
	if err == nil {
		t.Fatal("rerun should fail when repo is not initialized")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

func TestRerunStartsPipelineForCurrentBranch(t *testing.T) {
	setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := gate.Init(context.Background(), d, p, ".")
	if err != nil {
		t.Fatal(err)
	}

	branch, err := git.CurrentBranch(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	headSHA, err := git.HeadSHA(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, branch, headSHA, "0000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	run(t, ".", "git", "push", "no-mistakes", "HEAD:refs/heads/"+branch)

	startTestDaemon(t, p, d)

	out, err := executeCmd("rerun")
	if err != nil {
		t.Fatalf("rerun failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Rerun started") {
		t.Fatalf("expected rerun started output, got: %s", out)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var active ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Run == nil {
		t.Fatal("expected an active rerun")
	}
	if active.Run.Branch != branch {
		t.Fatalf("active branch = %q, want %q", active.Run.Branch, branch)
	}
	if active.Run.HeadSHA != headSHA {
		t.Fatalf("active head_sha = %q, want %q", active.Run.HeadSHA, headSHA)
	}
}

func TestRerunFromWorktreeUsesCurrentWorktreeBranch(t *testing.T) {
	repoDir := setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := gate.Init(context.Background(), d, p, repoDir)
	if err != nil {
		t.Fatal(err)
	}

	const branch = "wt-rerun-branch"
	run(t, repoDir, "git", "checkout", "-b", branch)
	run(t, repoDir, "git", "checkout", "-")

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, repoDir, "git", "worktree", "add", wtDir, branch)
	cleanupWorktree(t, repoDir, wtDir)

	headSHA, err := git.HeadSHA(context.Background(), wtDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, branch, headSHA, "0000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "push", "no-mistakes", "HEAD:refs/heads/"+branch)

	startTestDaemon(t, p, d)
	chdir(t, wtDir)

	out, err := executeCmd("rerun")
	if err != nil {
		t.Fatalf("rerun from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Rerun started") || !strings.Contains(out, branch) {
		t.Fatalf("expected rerun output for %q, got: %s", branch, out)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var active ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Run == nil {
		t.Fatal("expected an active rerun from worktree")
	}
	if active.Run.Branch != branch {
		t.Fatalf("active branch = %q, want %q", active.Run.Branch, branch)
	}
	if active.Run.HeadSHA != headSHA {
		t.Fatalf("active head_sha = %q, want %q", active.Run.HeadSHA, headSHA)
	}
}
