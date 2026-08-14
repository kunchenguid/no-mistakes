package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// mockWorkDirStep records the directory the pipeline executed it in, which is
// the run's worktree.
type mockWorkDirStep struct {
	name    types.StepName
	workDir chan string
}

func (s *mockWorkDirStep) Name() types.StepName { return s.name }
func (s *mockWorkDirStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.workDir <- sctx.WorkDir:
	default:
	}
	return &pipeline.StepOutcome{}, nil
}

// yamlPath quotes a path for YAML so a Windows drive letter is not read as a
// mapping and its separators survive as literal backslashes.
func yamlPath(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}

// configureWorktreeRoot points a checkout's run worktrees at root, the way an
// operator does in the global config, preserving whatever the test daemon
// already configured.
func configureWorktreeRoot(t *testing.T, p *paths.Paths, workingPath, root string) {
	t.Helper()
	existing, err := os.ReadFile(p.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	updated := string(existing)
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += "worktree_roots:\n  " + yamlPath(workingPath) + ": " + yamlPath(root) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunWorktreeIsCreatedInConfiguredRoot is the end of the operator's
// problem: a run worktree under NM_HOME inherits no directory-scoped toolchain
// configuration, so a repository with a worktree_roots entry must have its run
// created under that directory instead - and removed from it afterwards,
// leaving the operator's own files in the same directory untouched.
func TestRunWorktreeIsCreatedInConfiguredRoot(t *testing.T) {
	step := &mockWorkDirStep{name: types.StepReview, workDir: make(chan string, 1)}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, headSHA := setupTestGitRepo(t, p, d, "worktree-root-repo")
	root := filepath.Join(t.TempDir(), "repo-runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "mise.local.toml")
	if err := os.WriteFile(foreign, []byte("[tools]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configureWorktreeRoot(t, p, repo.WorkingPath, root)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("worktree-root-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	var executedIn string
	select {
	case executedIn = <-step.workDir:
	default:
		t.Fatal("step never ran, so no worktree was observed")
	}
	if want := filepath.Join(root, result.RunID); !samePath(executedIn, want) {
		t.Fatalf("step ran in %q, want %q", executedIn, want)
	}
	// The run records where it was placed, so nothing that looks at it later
	// has to ask the configuration again.
	if recorded := run.WorktreePath(); !samePath(recorded, executedIn) {
		t.Fatalf("run recorded worktree %q, want the directory it executed in %q", recorded, executedIn)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("operator file in the configured root must survive the run: %v", err)
	}
}

// TestCleanupOrphanWorktrees_ConfiguredRootRemovesOnlyRunDirectories is the
// startup-cleanup half of the same contract. The configured root belongs to
// the operator, so cleanup removes the leftovers of terminal runs and nothing
// else: not an active run's worktree, not their files, not their directories,
// and never the root itself.
func TestCleanupOrphanWorktrees_ConfiguredRootRemovesOnlyRunDirectories(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	workingPath := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(workingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo-runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("worktree_roots:\n  "+yamlPath(workingPath)+": "+yamlPath(root)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := d.InsertRepoWithID("repo1", workingPath, "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	activeRun, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	terminalRun, err := d.InsertRun(repo.ID, "old-branch", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminalRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	// Both runs record where they were placed, the way startRun does.
	for _, run := range []*db.Run{activeRun, terminalRun} {
		if err := d.SetRunWorktreeDir(run.ID, filepath.Join(root, run.ID)); err != nil {
			t.Fatal(err)
		}
	}

	activeWT := filepath.Join(root, activeRun.ID)
	terminalWT := filepath.Join(root, terminalRun.ID)
	operatorDir := filepath.Join(root, "scratch-checkout")
	for _, dir := range []string{activeWT, terminalWT, operatorDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	operatorFile := filepath.Join(root, "mise.local.toml")
	if err := os.WriteFile(operatorFile, []byte("[tools]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanWorktrees(d, p, recordedWorktreesOutsideDefaultRoot(d, p))

	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("terminal run worktree should have been cleaned up, stat err: %v", err)
	}
	for _, keep := range []string{root, activeWT, operatorDir, operatorFile} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("cleanup removed %q from the operator's worktree root: %v", keep, err)
		}
	}
}

// A repository without a worktree_roots entry keeps the default placement,
// which is what makes the feature invisible to everyone who does not use it.
func TestCleanupOrphanWorktrees_UnconfiguredRepoUsesDefaultRoot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	root := filepath.Join(t.TempDir(), "repo-runs")
	other := filepath.Join(t.TempDir(), "other-checkout")
	if err := os.WriteFile(p.ConfigFile(), []byte("worktree_roots:\n  "+yamlPath(other)+": "+yamlPath(root)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "checkout"), "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	terminalRun, err := d.InsertRun(repo.ID, "old-branch", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminalRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	terminalWT := p.WorktreeDir(repo.ID, terminalRun.ID)
	if err := os.MkdirAll(terminalWT, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanWorktrees(d, p, recordedWorktreesOutsideDefaultRoot(d, p))

	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("default-root worktree should have been cleaned up, stat err: %v", err)
	}
}

// TestDaemonRefusesToStartWithWorktreeRootInsideItsOwnWorktreesDirectory is the
// reviewer's scenario for the destructive misconfiguration: <NM_HOME>/worktrees
// holds one ULID-named directory per repository, a run ID is a ULID too, so a
// worktree root pointed at that directory would have every repository's
// directory read as a leftover run worktree - including the ones holding
// another repository's pending and running run worktrees. Config loading cannot
// catch it (it never learns where NM_HOME is), so the daemon refuses to start
// rather than starting and sweeping.
func TestDaemonRefusesToStartWithWorktreeRootInsideItsOwnWorktreesDirectory(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// A second repository with a live run, whose worktree the misread would
	// have deleted along with the directory holding it.
	victim, err := d.InsertRepoWithID("victimrepo", filepath.Join(t.TempDir(), "victim"), "https://example.com/owner/victim", "main")
	if err != nil {
		t.Fatal(err)
	}
	liveRun, err := d.InsertRun(victim.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(liveRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	liveWT := p.WorktreeDir(victim.ID, liveRun.ID)
	if err := os.MkdirAll(liveWT, 0o755); err != nil {
		t.Fatal(err)
	}

	misconfigured, err := d.InsertRepoWithID("repo1", filepath.Join(t.TempDir(), "checkout"), "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	configureWorktreeRoot(t, p, misconfigured.WorkingPath, p.WorktreesDir())

	err = RunWithResources(p, d)
	if err == nil {
		t.Fatal("daemon started with a worktree root inside its own worktrees directory")
	}
	if !strings.Contains(err.Error(), "worktree_roots") || !strings.Contains(err.Error(), p.WorktreesDir()) {
		t.Errorf("startup failure %q names neither the setting nor the offending directory", err)
	}
	if _, statErr := os.Stat(liveWT); statErr != nil {
		t.Errorf("another repository's live run worktree was removed: %v", statErr)
	}
	if _, statErr := os.Stat(p.Socket()); statErr == nil {
		t.Error("daemon bound its socket despite refusing the configured placement")
	}
}

// TestCleanupOrphanWorktrees_OperatorRootRemovesOnlyWhatARunRecorded is the
// other half of the same misconfiguration: a run-shaped directory in the
// operator's own directory is not evidence that a run created it. Cleanup there
// removes the exact directories run records name - whichever repository's run
// recorded them - and never enumerates the directory to guess at the rest.
func TestCleanupOrphanWorktrees_OperatorRootRemovesOnlyWhatARunRecorded(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	workingPath := filepath.Join(t.TempDir(), "checkout")
	root := filepath.Join(t.TempDir(), "repo-runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("worktree_roots:\n  "+yamlPath(workingPath)+": "+yamlPath(root)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := d.InsertRepoWithID("repo1", workingPath, "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	ownRun, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(ownRun.ID, filepath.Join(root, ownRun.ID)); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(ownRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	// A terminal run of a different repository that recorded a worktree in the
	// same directory, from before the operator reassigned it. Its own record
	// makes it ours to remove.
	otherRepo, err := d.InsertRepoWithID("repo2", filepath.Join(t.TempDir(), "other-checkout"), "https://example.com/owner/repo2", "main")
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := d.InsertRun(otherRepo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(otherRun.ID, filepath.Join(root, otherRun.ID)); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(otherRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	ownWT := filepath.Join(root, ownRun.ID)
	otherWT := filepath.Join(root, otherRun.ID)
	// Run-shaped, but no run record names it.
	unclaimedWT := filepath.Join(root, "01JZ8XQ7V6K9M3B0T5N2R4C8YD")
	// A run whose record names another directory entirely must not make this
	// one removable either.
	strayRun, err := d.InsertRun(repo.ID, "stray", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(strayRun.ID, filepath.Join(t.TempDir(), "elsewhere", strayRun.ID)); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(strayRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	strayWT := filepath.Join(root, strayRun.ID)
	for _, dir := range []string{ownWT, otherWT, unclaimedWT, strayWT} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cleanupOrphanWorktrees(d, p, recordedWorktreesOutsideDefaultRoot(d, p))

	for _, gone := range []string{ownWT, otherWT} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("recorded terminal run worktree %q should have been cleaned up, stat err: %v", gone, err)
		}
	}
	for _, keep := range []string{root, unclaimedWT, strayWT} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("cleanup removed %q, which no run record names: %v", keep, err)
		}
	}
}

// TestCleanupOrphanWorktrees_ReachesARootTheConfigNoLongerNames is the stale
// placement: a run executed in a directory the operator has since edited out of
// worktree_roots (or pointed elsewhere). Its worktree is still recorded, so the
// leftover must still be removed - deriving the search set from the live config
// instead would leave that directory behind forever, with nothing left to name
// it.
func TestCleanupOrphanWorktrees_ReachesARootTheConfigNoLongerNames(t *testing.T) {
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
	// No worktree_roots entry at all: this placement exists only on the run.
	abandonedRoot := filepath.Join(t.TempDir(), "former-runs")
	recordedWT := filepath.Join(abandonedRoot, run.ID)
	if err := os.MkdirAll(recordedWT, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(run.ID, recordedWT); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanWorktrees(d, p, recordedWorktreesOutsideDefaultRoot(d, p))

	if _, err := os.Stat(recordedWT); !os.IsNotExist(err) {
		t.Fatalf("recorded worktree in a root the config no longer names survived cleanup, stat err: %v", err)
	}
	if _, err := os.Stat(abandonedRoot); err != nil {
		t.Errorf("cleanup removed the operator's directory itself: %v", err)
	}
}

// TestStepDiff_ReadsThePlacementItsRunRecorded covers a config edit made while
// a run exists - exactly what `init --worktree-root` invites, since it prints
// an entry for the operator to paste in. The fix-review diff is served from the
// run's worktree on demand, so a re-derived placement would resolve a directory
// that never existed and fail the RPC the parked gate depends on.
func TestStepDiff_ReadsThePlacementItsRunRecorded(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	workingPath := filepath.Join(t.TempDir(), "checkout")
	repo, err := d.InsertRepoWithID("repo1", workingPath, "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}

	created := filepath.Join(t.TempDir(), "repo-runs", run.ID)
	if err := os.MkdirAll(created, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, created, "init")
	runGit(t, created, "config", "user.email", "test@example.com")
	runGit(t, created, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(created, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, created, "add", "tracked.txt")
	runGit(t, created, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(created, "tracked.txt"), []byte("agent fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(run.ID, created); err != nil {
		t.Fatal(err)
	}

	// The operator pastes a different root while the run is parked.
	configureWorktreeRoot(t, p, workingPath, filepath.Join(t.TempDir(), "somewhere-else"))

	diff, truncated, err := NewRunManager(d, p, nil).StepDiff(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("step diff after a mid-run placement edit: %v", err)
	}
	if truncated || !strings.Contains(diff, "agent fix") {
		t.Fatalf("diff = %q (truncated=%v), want the recorded worktree's change", diff, truncated)
	}
}

// TestPrepareRecoveredRun_LocatesThePlacementItsRunRecorded is the crash-recovery
// half: a parked run whose placement was re-derived from an edited config looks
// like a run whose worktree vanished, which fails it instead of resuming it.
func TestPrepareRecoveredRun_LocatesThePlacementItsRunRecorded(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, headSHA := setupTestGitRepo(t, p, d, "repo1")
	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	created := filepath.Join(t.TempDir(), "repo-runs", run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", created, headSHA)
	if err := d.SetRunWorktreeDir(run.ID, created); err != nil {
		t.Fatal(err)
	}
	// The operator points the checkout at a root this run knows nothing about.
	configureWorktreeRoot(t, p, repo.WorkingPath, filepath.Join(t.TempDir(), "somewhere-else"))

	m := NewRunManager(d, p, nil)
	layout, err := m.worktreeLayout()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.prepareRecoveredRun(context.Background(), layout, stored); err != nil && strings.Contains(err.Error(), "worktree is missing") {
		t.Fatalf("recovery lost the run's recorded worktree %q: %v", created, err)
	}
}

// TestReportUnusableWorktreeRoots_NamesEntriesThatDoNothing covers the silent
// failure mode of a path-keyed setting: an entry whose key does not match a
// registered checkout - a stale key after a move, a spelling this filesystem
// does not consider equal - places nothing at all, with no other symptom than
// runs continuing to appear under NM_HOME.
func TestReportUnusableWorktreeRoots_NamesEntriesThatDoNothing(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	registered := filepath.Join(t.TempDir(), "checkout")
	stale := filepath.Join(t.TempDir(), "moved-away")
	if _, err := d.InsertRepoWithID("repo1", registered, "https://example.com/owner/repo1", "main"); err != nil {
		t.Fatal(err)
	}
	configYAML := "worktree_roots:\n" +
		"  " + yamlPath(registered) + ": " + yamlPath(filepath.Join(t.TempDir(), "runs-a")) + "\n" +
		"  " + yamlPath(stale) + ": " + yamlPath(filepath.Join(t.TempDir(), "runs-b")) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldLogger)

	reportUnusableWorktreeRoots(d, startupWorktreeLayout(p))

	got := logs.String()
	if !strings.Contains(got, "matches no registered repository") {
		t.Errorf("startup did not report the stale worktree_roots entry, logs:\n%s", got)
	}
	if strings.Contains(got, registered) {
		t.Errorf("a matching entry must not be reported, logs:\n%s", got)
	}
}
