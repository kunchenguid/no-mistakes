//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixtureRootFromRepoRoot(t *testing.T) {
	root, err := fixtureRootFromRepoRoot(t.TempDir())
	if err == nil {
		t.Fatalf("fixtureRootFromRepoRoot succeeded with %q, want error", root)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	root, err = fixtureRootFromRepoRoot(repoRoot)
	if err != nil {
		t.Fatalf("fixtureRootFromRepoRoot: %v", err)
	}
	want := filepath.Join(repoRoot, "internal", "e2e", "fixtures")
	if root != want {
		t.Fatalf("fixture root = %q, want %q", root, want)
	}
}

func TestDaemonStartTimeoutLeavesRoomForLoginShellProbe(t *testing.T) {
	timeout, err := time.ParseDuration(e2eDaemonStartTimeout)
	if err != nil {
		t.Fatalf("parse e2eDaemonStartTimeout: %v", err)
	}
	if timeout <= 30*time.Second {
		t.Fatalf("e2eDaemonStartTimeout = %v, want more than the 30s login-shell probe budget", timeout)
	}
}

func TestCommitChangeCreatesMissingBranchFromMain(t *testing.T) {
	workDir := t.TempDir()
	h := &Harness{t: t, WorkDir: workDir}
	ctx := context.Background()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, workDir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	mustGit("init", "--initial-branch=main")
	mustGit("config", "user.email", "e2e@example.com")
	mustGit("config", "user.name", "E2E Test")
	mustGit("config", "commit.gpgsign", "false")

	readme := filepath.Join(workDir, "README.md")
	if err := os.WriteFile(readme, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	mustGit("add", "README.md")
	mustGit("commit", "-m", "initial commit")
	mainSHA := mustGit("rev-parse", "HEAD")

	mustGit("checkout", "-b", "feature/existing")
	featureOnly := filepath.Join(workDir, "feature-only.txt")
	if err := os.WriteFile(featureOnly, []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature-only.txt: %v", err)
	}
	mustGit("add", "feature-only.txt")
	mustGit("commit", "-m", "feature commit")

	h.CommitChange("feature/new", "hello.txt", "hello\n", "new branch commit")

	mergeBase := mustGit("merge-base", "feature/new", "main")
	if mergeBase != mainSHA {
		t.Fatalf("merge-base(feature/new, main) = %s, want %s", mergeBase, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(workDir, "feature-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-only.txt present on new branch, want branch rooted at main")
	}
	show := mustGit("show", "feature/new:hello.txt")
	if show != "hello" {
		t.Fatalf("hello.txt contents = %q, want %q", show, "hello")
	}
}

// TestHarnessGitNeutralizesInheritedCommitSigning pins the harness as the single
// owner of fixture commit signing. Journeys create repositories beyond
// h.WorkDir - clones, bare remotes, seed repos - and every commit into any of
// them must survive an operator's inherited signing config, which reaches the
// isolated HOME through a system gitconfig, GIT_CONFIG_GLOBAL, or GIT_CONFIG_*
// injection from an agent harness.
func TestHarnessGitNeutralizesInheritedCommitSigning(t *testing.T) {
	hostile := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(hostile, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = no-mistakes-missing-gpg-program\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", hostile)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "commit.gpgsign")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")

	ctx := context.Background()
	h := &Harness{t: t}
	identity := []string{
		"GIT_AUTHOR_NAME=E2E Test",
		"GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E Test",
		"GIT_COMMITTER_EMAIL=e2e@example.com",
	}
	rawGit := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), identity...)
		return cmd.Run()
	}
	seed := func(dir string, git func(dir string, args ...string) error) error {
		if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
			return err
		}
		if err := git(dir, "add", "seed.txt"); err != nil {
			return err
		}
		return git(dir, "commit", "-m", "seed")
	}

	control := t.TempDir()
	if err := rawGit(control, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("init control repo: %v", err)
	}
	if err := seed(control, rawGit); err == nil {
		t.Fatal("hostile inherited commit signing unexpectedly allowed an unguarded commit")
	}

	harnessGit := func(dir string, args ...string) error {
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			return fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
		return nil
	}
	// A clone, not an init: git clone never copies the source repo's local
	// config, which is how a fixture repository outside h.WorkDir loses every
	// repo-local guard.
	fixture := filepath.Join(t.TempDir(), "fixture-clone")
	if err := harnessGit(filepath.Dir(fixture), "clone", control, fixture); err != nil {
		t.Fatalf("clone fixture repo: %v", err)
	}
	if err := seed(fixture, harnessGit); err != nil {
		t.Fatalf("harness fixture repo inherited hostile commit signing: %v", err)
	}
}

func TestDaemonStopDirFallsBackWhenWorkDirMoved(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "missing-work")
	homeDir := t.TempDir()
	h := &Harness{WorkDir: workDir, HomeDir: homeDir}

	if got := h.daemonStopDir(); got != homeDir {
		t.Fatalf("daemonStopDir() = %q, want home dir %q", got, homeDir)
	}
}

func TestWaitForRunPrefersNewestRunOnBranch(t *testing.T) {
	nmHome, err := os.MkdirTemp("/tmp", "nm-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
	workDir := t.TempDir()
	p := paths.WithRoot(nmHome)
	if err := os.MkdirAll(nmHome, 0o755); err != nil {
		t.Fatalf("mkdir nm home: %v", err)
	}

	server := ipc.NewServer()
	var calls atomic.Int32
	server.Handle(ipc.MethodGetRuns, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		if calls.Add(1) == 1 {
			return ipc.GetRunsResult{Runs: []ipc.RunInfo{
				{ID: "run-new", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunRunning, CreatedAt: 20, UpdatedAt: 20},
				{ID: "run-old", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 10, UpdatedAt: 10},
			}}, nil
		}
		return ipc.GetRunsResult{Runs: []ipc.RunInfo{
			{ID: "run-new", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 20, UpdatedAt: 30},
			{ID: "run-old", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 10, UpdatedAt: 10},
		}}, nil
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(p.Socket())
	}()
	t.Cleanup(func() {
		server.Close()
		if err := <-errCh; err != nil {
			t.Errorf("ipc server: %v", err)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	h := &Harness{t: t, NMHome: nmHome, WorkDir: workDir}
	run := h.WaitForRun("feature/e2e", 2*time.Second)
	if run.ID != "run-new" {
		t.Fatalf("WaitForRun returned %q, want newest run", run.ID)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("WaitForRun status = %s, want %s", run.Status, types.RunCompleted)
	}
	if calls.Load() < 2 {
		t.Fatalf("GetRuns calls = %d, want at least 2 polls", calls.Load())
	}
}
