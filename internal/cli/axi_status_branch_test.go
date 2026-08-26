package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func TestAxiStatusForeignRunGateHelpCannotMutateCurrentBranch(t *testing.T) {
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
	mineStep, err := database.InsertStepResult(mine.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert current-branch step: %v", err)
	}
	if err := database.UpdateStepStatus(mineStep.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("park current-branch step: %v", err)
	}

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("start other-branch run: %v", err)
	}
	otherStep, err := database.InsertStepResult(other.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert other-branch step: %v", err)
	}
	if err := database.UpdateStepStatus(otherStep.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("park other-branch step: %v", err)
	}
	if err := database.SetStepFindings(otherStep.ID, findingsJSON(t, nil, "other branch gate")); err != nil {
		t.Fatalf("set other-branch findings: %v", err)
	}

	out := axiStatusOutput(t, other.ID)
	for _, want := range []string{
		"other_branch_run:",
		"gate:",
		"other branch gate",
		"inspection-only",
		"no run-scoped response command exists",
		"axi logs --run " + other.ID + " --step review --full",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("foreign-run gate status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "axi respond") {
		t.Fatalf("foreign-run gate status offered a branch-scoped mutation command:\n%s", out)
	}
}

func TestAxiStatusExplicitOlderSameBranchRunGateIsInspectionOnly(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	older, err := database.InsertRun(repo.ID, "feature/mine", "head-older", "base")
	if err != nil {
		t.Fatalf("insert older current-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(older.ID, types.RunRunning); err != nil {
		t.Fatalf("start older current-branch run: %v", err)
	}
	olderStep, err := database.InsertStepResult(older.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert older current-branch step: %v", err)
	}
	if err := database.UpdateStepStatus(olderStep.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("park older current-branch step: %v", err)
	}
	if err := database.SetStepFindings(olderStep.ID, findingsJSON(t, nil, "older run gate")); err != nil {
		t.Fatalf("set older current-branch findings: %v", err)
	}

	newer, err := database.InsertRun(repo.ID, "feature/mine", "head-newer", "base")
	if err != nil {
		t.Fatalf("insert newer current-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(newer.ID, types.RunRunning); err != nil {
		t.Fatalf("start newer current-branch run: %v", err)
	}

	implicit := decodeStatusDoc(t, axiStatusOutput(t, ""))
	if implicit.Run.ID != newer.ID {
		t.Fatalf("bare status resolved run %q, want newer active run %s", implicit.Run.ID, newer.ID)
	}

	out := axiStatusOutput(t, older.ID)
	doc := decodeStatusDoc(t, out)
	if doc.Run.ID != older.ID {
		t.Fatalf("explicit status resolved run %q, want older run %s:\n%s", doc.Run.ID, older.ID, out)
	}
	if strings.Contains(out, "axi respond") {
		t.Fatalf("explicit older-run status offered a bare command that would target newer run %s:\n%s", newer.ID, out)
	}
	for _, want := range []string{
		"gate:",
		"older run gate",
		"inspection-only",
		"no run-scoped response command exists",
		"axi logs --run " + older.ID + " --step review --full",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspection-only explicit status missing %q:\n%s", want, out)
		}
	}
	for _, unsafe := range []string{"answer it", "own worktree/branch"} {
		if strings.Contains(out, unsafe) {
			t.Fatalf("inspection-only explicit status included unsafe guidance %q:\n%s", unsafe, out)
		}
	}
}

func TestAxiStatusExplicitRunWithUnknownCallerBranchCannotOfferMutationCommands(t *testing.T) {
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
	mineStep, err := database.InsertStepResult(mine.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert current-branch step: %v", err)
	}
	if err := database.UpdateStepStatus(mineStep.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("park current-branch step: %v", err)
	}

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("start other-branch run: %v", err)
	}
	otherStep, err := database.InsertStepResult(other.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert other-branch step: %v", err)
	}
	if err := database.UpdateStepStatus(otherStep.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("park other-branch step: %v", err)
	}
	if err := database.SetStepFindings(otherStep.ID, findingsJSON(t, nil, "other branch gate")); err != nil {
		t.Fatalf("set other-branch findings: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	if _, err := runAxiStatus(cmd, other.ID); err != nil {
		t.Fatalf("explicit axi status after branch lookup failure: %v\n%s", err, out.String())
	}
	doc := decodeStatusDoc(t, out.String())
	if doc.Run.ID != other.ID {
		t.Fatalf("explicit run with unknown caller branch must retain run label, got:\n%s", out.String())
	}
	if doc.OtherBranchRun.ID != "" {
		t.Fatalf("status asserted an unproven branch relationship:\n%s", out.String())
	}
	if strings.Contains(out.String(), "axi respond") {
		t.Fatalf("explicit run without proven caller-branch ownership offered a mutation command:\n%s", out.String())
	}
	for _, want := range []string{
		"gate:",
		"other branch gate",
		"inspection-only",
		"no run-scoped response command exists",
		"axi logs --run " + other.ID + " --step review --full",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inspection-only gate status missing %q:\n%s", want, out.String())
		}
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

	got, _, err := resolveRun(&axiEnv{d: database, repo: repo}, "", "feature/mine")
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

func TestAxiLogsExplicitRunTailHelpKeepsRunID(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	other, err := database.InsertRun(repo.ID, "feature/other", "head-other", "base")
	if err != nil {
		t.Fatalf("insert other-branch run: %v", err)
	}
	if err := database.UpdateRunStatus(other.ID, types.RunRunning); err != nil {
		t.Fatalf("start other-branch run: %v", err)
	}
	logDir := p.RunLogDir(other.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "review.log"), []byte(strings.Repeat("line\n", logTailLines+1)), 0o644); err != nil {
		t.Fatalf("write review log: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiLogs(cmd, "review", other.ID, false); err != nil {
		t.Fatalf("axi logs explicit run: %v\n%s", err, out.String())
	}
	want := "axi logs --run " + other.ID + " --step review --full"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("explicit-run tail help lost selected run identity %q:\n%s", want, out.String())
	}
}

func TestAxiLogsUnknownExplicitRunIDReportsNotFound(t *testing.T) {
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

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiLogs(cmd, "review", "missing-run", false); err == nil {
		t.Fatalf("axi logs unexpectedly found missing explicit run:\n%s", out.String())
	}
	var doc struct {
		Error string `toon:"error"`
	}
	if err := toon.UnmarshalString(out.String(), &doc); err != nil {
		t.Fatalf("decode axi logs error: %v\n%s", err, out.String())
	}
	if doc.Error != `run "missing-run" not found` {
		t.Fatalf("error = %q, want exact explicit-run not-found error", doc.Error)
	}
	if strings.Contains(out.String(), "no run found for this branch") || strings.Contains(out.String(), "axi run --intent") {
		t.Fatalf("missing explicit run was reported as current-branch absence:\n%s", out.String())
	}
}

func TestAxiStatusNoRunRenderingUsesResolutionSnapshot(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	got, runs, err := resolveRun(&axiEnv{d: database, repo: repo}, "", "feature/mine")
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %#v, want no run", got)
	}

	inserted, err := database.InsertRun(repo.ID, "feature/mine", "head-mine", "base")
	if err != nil {
		t.Fatalf("insert current-branch run: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if _, err := emitNoRunForCaller(cmd, &axiEnv{d: database, repo: repo}, "feature/mine", runs); err != nil {
		t.Fatalf("render no-run status: %v", err)
	}
	if strings.Contains(out.String(), inserted.ID) {
		t.Fatalf("no-run rendering included a run created after resolution:\n%s", out.String())
	}
}

func TestAxiStatusBranchLookupFailureIsNotDetachedHEAD(t *testing.T) {
	repoDir, _, _, _ := setupAxiQueryRepo(t)
	chdir(t, repoDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	if _, err := runAxiStatus(cmd, ""); err == nil {
		t.Fatalf("axi status succeeded after branch lookup failed:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "context canceled") {
		t.Fatalf("axi status did not surface the branch lookup failure:\n%s", got)
	}
	if strings.Contains(got, "detached HEAD") || strings.Contains(got, "runs_on_current_branch") {
		t.Fatalf("axi status reported a branch lookup failure as no-run detached HEAD:\n%s", got)
	}
}

func TestAxiDetachedHEADHelpOffersOnlyValidActions(t *testing.T) {
	repoDir, _, _, _ := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "--detach")
	chdir(t, repoDir)

	t.Run("status", func(t *testing.T) {
		out := axiStatusOutput(t, "")
		for _, want := range []string{"current_branch: unknown", "no current branch", "axi status --run <id>", "check out a branch"} {
			if !strings.Contains(out, want) {
				t.Fatalf("detached status missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "axi run --intent") {
			t.Fatalf("detached status suggested starting a run:\n%s", out)
		}
	})

	t.Run("logs", func(t *testing.T) {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&out)
		if _, err := runAxiLogs(cmd, "review", "", false); err == nil {
			t.Fatalf("detached logs unexpectedly found a run:\n%s", out.String())
		}
		got := out.String()
		for _, want := range []string{"no current branch", "axi logs --run <id> --step <step>", "check out a branch"} {
			if !strings.Contains(got, want) {
				t.Fatalf("detached logs help missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "axi run --intent") {
			t.Fatalf("detached logs suggested starting a run:\n%s", got)
		}
	})
}
