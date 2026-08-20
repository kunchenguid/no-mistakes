//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSweepOrphanRunProcessesReapsFinishedRunAndSparesActiveOne is the daemon
// wiring for the leaked-process class: a predecessor daemon that died mid-run
// never got to tear anything down, so its children are still standing in
// worktrees that no run owns. Startup must reap those, and must leave alone
// anything standing in a worktree whose run is still pending or running -
// killing there would take down a live pipeline.
func TestSweepOrphanRunProcessesReapsFinishedRunAndSparesActiveOne(t *testing.T) {
	// The startup sweep normally refuses to touch anything young, so a run
	// starting concurrently with daemon startup is never mistaken for a leak.
	// These fixtures are seconds old, so the floor is lowered for the test.
	orig := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = orig })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	activeRun, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if activeRun.Status != types.RunPending {
		t.Fatalf("expected new run to default to pending, got %s", activeRun.Status)
	}
	finishedRun, err := d.InsertRun(repo.ID, "old-branch", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(finishedRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	activePID := startOrphanInWorktree(t, p.WorktreeDir(repo.ID, activeRun.ID))
	leakedPID := startOrphanInWorktree(t, p.WorktreeDir(repo.ID, finishedRun.ID))

	sweepOrphanRunProcesses(d, p, sweepableWorktrees(leftoverRecordedRunWorktrees(d, p), activeRecordedRunWorktrees(d, p)))

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the finished run's worktree survived the startup sweep", leakedPID)
	}
	if !processIsAlive(activePID) {
		t.Fatalf("orphan %d in an active run's worktree must not be swept", activePID)
	}
}

// TestSweepOrphanRunProcessesReachesRecordedWorktreeAndSparesUnclaimedOnes is
// the startup sweep in an operator's own directory (worktree_roots). Two
// properties hold there, and both come from the run records rather than the
// configuration: a leaked process in a recorded worktree is reaped even after
// the operator edited that root out of the config - nothing else would ever
// name that directory again - while a process standing in a directory no run
// recorded is out of reach, exactly as cleanup refuses to remove one.
func TestSweepOrphanRunProcessesReachesRecordedWorktreeAndSparesUnclaimedOnes(t *testing.T) {
	orig := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = orig })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "checkout"), "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	finishedRun, err := d.InsertRun(repo.ID, "old-branch", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	activeRun, err := d.InsertRun(repo.ID, "feature", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}

	// The root lives only on the run records: the config names nothing.
	abandonedRoot := filepath.Join(t.TempDir(), "former-runs")
	finishedWT := filepath.Join(abandonedRoot, finishedRun.ID)
	activeWT := filepath.Join(abandonedRoot, activeRun.ID)
	for _, spec := range []struct {
		id  string
		dir string
	}{{finishedRun.ID, finishedWT}, {activeRun.ID, activeWT}} {
		if err := d.SetRunWorktreeDir(spec.id, spec.dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunStatus(finishedRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(activeRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	leakedPID := startOrphanInWorktree(t, finishedWT)
	activePID := startOrphanInWorktree(t, activeWT)
	operatorPID := startOrphanInWorktree(t, filepath.Join(abandonedRoot, "scratch-checkout"))
	unclaimedPID := startOrphanInWorktree(t, filepath.Join(abandonedRoot, "01JZ8XQ7V6K9M3B0T5N2R4C8YD"))

	sweepOrphanRunProcesses(d, p, sweepableWorktrees(leftoverRecordedRunWorktrees(d, p), activeRecordedRunWorktrees(d, p)))

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in a recorded worktree the config no longer names survived the startup sweep", leakedPID)
	}
	if !processIsAlive(activePID) {
		t.Errorf("orphan %d in an active run's worktree must not be swept", activePID)
	}
	if !processIsAlive(operatorPID) {
		t.Errorf("the sweep signalled %d in the operator's own directory", operatorPID)
	}
	if !processIsAlive(unclaimedPID) {
		t.Errorf("the sweep signalled %d in a run-shaped directory no run recorded", unclaimedPID)
	}
}

// TestSweepRunWorktreeProcessesReapsLeakedChildAtRunCleanup covers the other
// call site: when a run's goroutine finishes, anything still standing in that
// run's worktree is terminated before the directory is removed, and another
// run's worktree is never touched.
func TestSweepRunWorktreeProcessesReapsLeakedChildAtRunCleanup(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(nil, p, nil)

	finished := p.WorktreeDir("repo1", "run1")
	other := p.WorktreeDir("repo1", "run2")
	leakedPID := startOrphanInWorktree(t, finished)
	otherPID := startOrphanInWorktree(t, other)

	m.sweepRunWorktreeProcesses("repo1", "run1", finished)

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the finished run's worktree survived run cleanup", leakedPID)
	}
	if !processIsAlive(otherPID) {
		t.Fatalf("run cleanup reached another run's worktree and killed %d", otherPID)
	}
}

// TestSweepRunWorktreeProcessesReapsLeakedChildInTheRunsRecordedRoot covers a
// run the operator placed in a directory of their own (worktree_roots): the
// reaper's reach is the directory that run was actually created in, not
// anything the configuration says - it may have been edited, or removed, while
// the run was executing. Everything else in that directory is out of reach,
// including a directory that looks just like another run's worktree.
func TestSweepRunWorktreeProcessesReapsLeakedChildInTheRunsRecordedRoot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(nil, p, nil)

	root := filepath.Join(t.TempDir(), "repo-runs")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	finished := filepath.Join(root, runID)
	operatorDir := filepath.Join(root, "scratch-checkout")
	unclaimed := filepath.Join(root, "01JZ8XQ7V6K9M3B0T5N2R4C8YF")
	leakedPID := startOrphanInWorktree(t, finished)
	operatorPID := startOrphanInWorktree(t, operatorDir)
	unclaimedPID := startOrphanInWorktree(t, unclaimed)

	m.sweepRunWorktreeProcesses("repo1", runID, finished)

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the finished run's configured worktree survived run cleanup", leakedPID)
	}
	if !processIsAlive(operatorPID) {
		t.Fatalf("run cleanup signalled %d in the operator's own directory", operatorPID)
	}
	if !processIsAlive(unclaimedPID) {
		t.Fatalf("run cleanup signalled %d in a run-shaped directory this run does not own", unclaimedPID)
	}
}

// TestResumeRecoveredRunSweepsWorktreeProcessesBeforeRemoval covers the run that
// this daemon did not start: a crash-recovered one. Its cleanup removes the
// worktree just as a normal run's does, so it must sweep first for the same
// reason - a descendant that called setsid(2) survives with the removed
// directory as its cwd, and outside the default worktrees tree nothing can name
// that directory again once it is gone and the run is terminal.
func TestResumeRecoveredRunSweepsWorktreeProcessesBeforeRemoval(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "checkout"), "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	// Placed in a directory of the operator's own, the way worktree_roots does.
	root := filepath.Join(t.TempDir(), "repo-runs")
	wtDir := filepath.Join(root, run.ID)
	if err := d.SetRunWorktreeDir(run.ID, wtDir); err != nil {
		t.Fatal(err)
	}
	leakedPID := startOrphanInWorktree(t, wtDir)
	operatorPID := startOrphanInWorktree(t, filepath.Join(root, "scratch-checkout"))

	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	m.resumeRecoveredRun(recoveredRunPlan{
		run:     stored,
		repo:    repo,
		workDir: wtDir,
		gateDir: p.RepoDir(repo.ID),
		cfg:     config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}),
		agent:   agent.NewNoop(),
		steps:   []pipeline.Step{&mockPassStep{name: types.StepReview}},
	})
	m.Shutdown()

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in a resumed run's worktree survived its cleanup", leakedPID)
	}
	if !processIsAlive(operatorPID) {
		t.Errorf("cleanup signalled %d in the operator's own directory", operatorPID)
	}
}

// TestRemoveRunWorktreeSweepsBeforeRemoving pins the ordering every removal path
// depends on. It is one helper precisely because the ordering is invisible when
// forgotten: the process outlives the directory, and outside the default tree
// nothing can name that directory again once the run is terminal and the
// directory is gone.
func TestRemoveRunWorktreeSweepsBeforeRemoving(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")

	root := filepath.Join(t.TempDir(), "repo-runs")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(root, runID)
	gitCmd(t, p.RepoDir("repo1"), "worktree", "add", "--detach", wtDir, headSHA)
	leakedPID := startOrphanInWorktree(t, wtDir)
	operatorPID := startOrphanInWorktree(t, filepath.Join(root, "scratch-checkout"))

	NewRunManager(d, p, nil).removeRunWorktree("repo1", runID, p.RepoDir("repo1"), wtDir, "test")

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the removed worktree survived removal", leakedPID)
	}
	if !processIsAlive(operatorPID) {
		t.Errorf("removal signalled %d in the operator's own directory", operatorPID)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory survived removal, stat err: %v", err)
	}
}

// TestRunSetupFailureSweepsTheWorktreeItRemoves is the reachable path that
// motivated the shared helper: setup fails after the worktree exists, and a
// process the setup itself left standing in it (a git hook that backgrounds a
// child, an SSH control master, a credential helper) must not be stranded by the
// removal that follows.
func TestRunSetupFailureSweepsTheWorktreeItRemoves(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	repo, _ := setupTestGitRepo(t, p, d, "setup-failure-repo")
	root := filepath.Join(t.TempDir(), "repo-runs")
	configureWorktreeRoot(t, p, repo.WorkingPath, root)

	// A post-checkout hook fires inside the new worktree when the daemon creates
	// it, and leaves a process standing there whose parent exits immediately.
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	gateHooks := filepath.Join(p.RepoDir("setup-failure-repo"), "hooks")
	if err := os.MkdirAll(gateHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nsleep 300 >/dev/null 2>&1 &\necho $! > " + pidFile + "\n"
	if err := os.WriteFile(filepath.Join(gateHooks, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	// Setup then fails after the worktree exists: the pushed tree's repo config
	// does not parse.
	headSHA := commitInvalidRepoConfig(t, repo.WorkingPath)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	pushErr := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("setup-failure-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if pushErr == nil {
		t.Fatal("push with an unparseable repo config created a run")
	}

	leakedPID := readOrphanPID(t, pidFile)
	if leakedPID == 0 {
		t.Skip("post-checkout hook did not run, so no orphan was planted in the worktree")
	}
	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d planted in the worktree survived the setup-failure removal", leakedPID)
	}
}

// commitInvalidRepoConfig pushes a commit whose .no-mistakes.yaml cannot be
// parsed, which fails run setup after the worktree has been created.
func commitInvalidRepoConfig(t *testing.T, workDir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("auto_fix: [not, a, mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".no-mistakes.yaml")
	gitCmd(t, workDir, "commit", "-m", "invalid config")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")
	return gitOutput(t, workDir, "rev-parse", "HEAD")
}

// readOrphanPID waits briefly for the hook to record its background child, and
// reports 0 when the hook never ran.
func readOrphanPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0
}

// startOrphanInWorktree leaves a real long-lived process standing in dir whose
// parent has already exited, which is exactly the shape a leaked pipeline
// child has by the time anyone notices it.
func startOrphanInWorktree(t *testing.T, dir string) int {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		t.Fatalf("orphan pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !processIsAlive(pid) {
		t.Fatalf("orphan %d was not running", pid)
	}
	return pid
}

func processIsAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !processIsAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processIsAlive(pid)
}
