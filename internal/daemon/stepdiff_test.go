package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The fix-review diff is the one piece of gate context that is not persisted
// anywhere, so it cannot be reconstructed from get_run. It is therefore served
// on demand from the run's worktree instead of riding the event stream, where
// a large diff would exceed the frame limit and take the whole subscription
// down with it.

func stepDiffFixture(t *testing.T, contents string) (*RunManager, string) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepoWithID("testrepo", filepath.Join(root, "clone"), "https://example.test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}

	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "init")
	runGit(t, worktree, "config", "user.email", "test@example.com")
	runGit(t, worktree, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "tracked.txt")
	runGit(t, worktree, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewRunManager(database, p, nil)
	return m, run.ID
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestStepDiff_ReturnsTheWorktreeDiffOnDemand(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small diff reported as truncated")
	}
	if !strings.Contains(diff, "tracked.txt") || !strings.Contains(diff, "agent fix") {
		t.Fatalf("diff did not describe the change:\n%s", diff)
	}
}

// A diff larger than the response budget is cut rather than returned whole.
// An oversized response would blow the transport frame limit, which is exactly
// the failure this RPC exists to avoid.
func TestStepDiff_BoundsAnOversizedDiff(t *testing.T) {
	huge := strings.Repeat("a very long changed line that repeats\n", 60_000)
	m, runID := stepDiffFixture(t, huge)

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("oversized diff was not reported as truncated")
	}
	if len(diff) > maxStepDiffBytes {
		t.Fatalf("diff length = %d, want <= %d", len(diff), maxStepDiffBytes)
	}
	if !strings.Contains(diff, "tracked.txt") {
		t.Fatalf("truncated diff lost its leading context:\n%s", diff[:200])
	}
}

func TestStepDiff_UnknownRunFailsClosed(t *testing.T) {
	m, _ := stepDiffFixture(t, "agent fix\n")
	if _, _, err := m.StepDiff(context.Background(), "01NOSUCHRUN"); err == nil {
		t.Fatal("expected an error for an unknown run")
	}
}

// The fix-review gate depends on this RPC, and where the run's worktree is is
// recorded on the run - so an unrelated fault in the global config must not take
// the diff down with it. An operator who mistypes YAML while a run is parked
// would otherwise lose the gate's diff for a reason that has nothing to do with
// the run.
func TestStepDiff_ServesTheDiffWhileTheGlobalConfigIsUnreadable(t *testing.T) {
	m, runID := stepDiffFixture(t, "agent fix\n")
	if err := os.WriteFile(m.paths.ConfigFile(), []byte("worktree_roots: [not, a, mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, truncated, err := m.StepDiff(context.Background(), runID)
	if err != nil {
		t.Fatalf("step diff with an unreadable global config: %v", err)
	}
	if truncated || !strings.Contains(diff, "agent fix") {
		t.Fatalf("diff = %q (truncated=%v), want the worktree's change", diff, truncated)
	}
}
