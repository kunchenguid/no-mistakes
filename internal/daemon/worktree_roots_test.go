package daemon

import (
	"bytes"
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

	cleanupOrphanWorktrees(d, p, startupWorktreeLayout(p))

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

	cleanupOrphanWorktrees(d, p, startupWorktreeLayout(p))

	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("default-root worktree should have been cleaned up, stat err: %v", err)
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

	reportUnusableWorktreeRoots(d, p, startupWorktreeLayout(p))

	got := logs.String()
	if !strings.Contains(got, "matches no registered repository") {
		t.Errorf("startup did not report the stale worktree_roots entry, logs:\n%s", got)
	}
	if strings.Contains(got, registered) {
		t.Errorf("a matching entry must not be reported, logs:\n%s", got)
	}
}
