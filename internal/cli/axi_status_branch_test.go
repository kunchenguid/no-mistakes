package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// statusDoc is the machine-readable shape a driving agent parses out of
// `axi status`: a run it may treat as this worktree's under `run:`, and a run
// that is provably not this worktree's under `other_branch_run:`.
type statusDoc struct {
	CurrentBranch string `toon:"current_branch"`
	Run           struct {
		ID     string `toon:"id"`
		Branch string `toon:"branch"`
		Status string `toon:"status"`
	} `toon:"run"`
	OtherBranchRun struct {
		ID     string `toon:"id"`
		Branch string `toon:"branch"`
		Status string `toon:"status"`
	} `toon:"other_branch_run"`
}

func decodeStatusDoc(t *testing.T, out string) statusDoc {
	t.Helper()
	var doc statusDoc
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatalf("decode axi status TOON: %v\n%s", err, out)
	}
	return doc
}

func axiStatusOutput(t *testing.T, runID string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiStatus(cmd, runID); err != nil {
		t.Fatalf("axi status: %v\n%s", err, out.String())
	}
	return out.String()
}

// TestAxiStatusNeverReportsAnotherBranchesRunAsThisWorktrees is the regression
// for the observed defect: several worktrees of one repository, each on its own
// branch, all read back one unrelated branch's cancelled run under the plain
// `run:` key, with nothing marking it as somebody else's. A supervising agent
// judging validation by that read concludes the work failed while its real
// pipeline is still in flight.
func TestAxiStatusNeverReportsAnotherBranchesRunAsThisWorktrees(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
		t.Fatalf("cancel other-branch run: %v", err)
	}

	out := axiStatusOutput(t, "")
	doc := decodeStatusDoc(t, out)
	if doc.Run.ID != "" {
		t.Fatalf("status claimed run %s (branch %q) as this worktree's while this branch has no run:\n%s",
			doc.Run.ID, doc.Run.Branch, out)
	}
	if doc.OtherBranchRun.ID != "" {
		t.Fatalf("status must not silently resolve another branch's run without --run, got %s:\n%s",
			doc.OtherBranchRun.ID, out)
	}
	if doc.CurrentBranch != "feature/mine" {
		t.Fatalf("current_branch = %q, want feature/mine:\n%s", doc.CurrentBranch, out)
	}
	// The reading stays useful: the other branch's run is still listed, so
	// `--run <id>` can inspect it deliberately.
	for _, want := range []string{other.ID, "feature/other", "--run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q in:\n%s", want, out)
		}
	}
}

// TestAxiStatusReportsThisBranchesOwnRun keeps the correct path intact: when
// the worktree's branch does have a run, status reports it under `run:`.
func TestAxiStatusReportsThisBranchesOwnRun(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	mine, err := database.InsertRun(repo.ID, "feature/mine", "head-mine", "base")
	if err != nil {
		t.Fatalf("insert current-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(mine.ID, types.RunRunning); err != nil {
		t.Fatalf("start current-branch run: %v", err)
	}
	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
		t.Fatalf("cancel other-branch run: %v", err)
	}

	out := axiStatusOutput(t, "")
	doc := decodeStatusDoc(t, out)
	if doc.Run.ID != mine.ID {
		t.Fatalf("run.id = %q, want this branch's run %s:\n%s", doc.Run.ID, mine.ID, out)
	}
	if doc.OtherBranchRun.ID != "" {
		t.Fatalf("status marked this branch's own run as another branch's:\n%s", out)
	}
}

// TestAxiStatusExplicitRunIDStillInspectsAnotherBranchesRun keeps deliberate
// inspection working, but under a key that tells a parser the run is not this
// worktree's.
func TestAxiStatusExplicitRunIDStillInspectsAnotherBranchesRun(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
		t.Fatalf("cancel other-branch run: %v", err)
	}

	out := axiStatusOutput(t, other.ID)
	doc := decodeStatusDoc(t, out)
	if doc.OtherBranchRun.ID != other.ID {
		t.Fatalf("explicit --run must still render the run under other_branch_run, got:\n%s", out)
	}
	if doc.OtherBranchRun.Status != "cancelled" {
		t.Fatalf("explicit --run lost the run detail, status = %q:\n%s", doc.OtherBranchRun.Status, out)
	}
	if doc.Run.ID != "" {
		t.Fatalf("another branch's run must not be presented as this worktree's run:\n%s", out)
	}
	if doc.CurrentBranch != "feature/mine" {
		t.Fatalf("current_branch = %q, want feature/mine:\n%s", doc.CurrentBranch, out)
	}
}

// TestResolveRunDoesNotFallBackToAnotherBranch pins the cause at its owner:
// with the caller's branch known and no run on it, resolution reports no run
// rather than the repository's most recent run on some other branch.
func TestResolveRunDoesNotFallBackToAnotherBranch(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("start run: %v", err)
	}

	got, err := resolveRun(&axiEnv{d: database, repo: repo}, "", "feature/mine")
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %s on branch %q, want no run for feature/mine", got.ID, got.Branch)
	}
}

// TestAxiLogsDoesNotReadAnotherBranchesRunLogs covers the second consumer of
// the same resolution: reading a foreign run's step log is the same misreport.
func TestAxiLogsDoesNotReadAnotherBranchesRunLogs(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
		t.Fatalf("cancel other-branch run: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiLogs(cmd, "review", "", false); err == nil {
		t.Fatalf("axi logs resolved another branch's run:\n%s", out.String())
	}
	got := out.String()
	if strings.Contains(got, other.ID) {
		t.Fatalf("axi logs pointed at another branch's run %s:\n%s", other.ID, got)
	}
	if !strings.Contains(got, "--run") {
		t.Fatalf("axi logs should point at deliberate --run inspection:\n%s", got)
	}
}
