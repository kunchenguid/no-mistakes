package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPreviousBranchStepHistoryReturnsMostRecentPriorRunOnSameBranch(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history", "https://example.com/repo.git", "main")

	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"older-1","severity":"warning","description":"older"}]}`)
	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"newer-1","severity":"error","description":"newer"}]}`)
	current, _ := d.InsertRun(repo.ID, "feature", "head3", "base")

	history, err := d.PreviousBranchStepHistory(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step history: %v", err)
	}
	if history == nil || len(history.Rounds) != 1 {
		t.Fatalf("want rounds from exactly the newest prior run, got %#v", history)
	}
	if *history.Rounds[0].FindingsJSON != `{"findings":[{"id":"newer-1","severity":"error","description":"newer"}]}` {
		t.Fatalf("returned rounds from the wrong run: %s", *history.Rounds[0].FindingsJSON)
	}
}

// The previous run's terminal status is the only evidence of whether the user
// finished deciding about its findings, so it must travel with the rounds.
func TestPreviousBranchStepHistoryCarriesPriorRunStatus(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-status", "https://example.com/repo.git", "main")

	prior := insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"f-1","severity":"warning","description":"d"}]}`)
	if err := d.UpdateRunStatus(prior, types.RunFailed); err != nil {
		t.Fatalf("update run status: %v", err)
	}
	current, _ := d.InsertRun(repo.ID, "feature", "head-current", "base")

	history, err := d.PreviousBranchStepHistory(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step history: %v", err)
	}
	if history == nil {
		t.Fatal("want history for the prior run")
	}
	if history.RunID != prior {
		t.Errorf("run id = %q, want %q", history.RunID, prior)
	}
	if history.RunStatus != types.RunFailed {
		t.Errorf("run status = %q, want %q", history.RunStatus, types.RunFailed)
	}
}

func TestPreviousBranchStepHistoryExcludesCurrentRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-self", "https://example.com/repo.git", "main")
	current := insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"self-1","severity":"warning","description":"self"}]}`)

	history, err := d.PreviousBranchStepHistory(repo.ID, "feature", types.StepReview, current)
	if err != nil {
		t.Fatalf("previous branch step history: %v", err)
	}
	if history != nil {
		t.Fatalf("current run must never be its own prior history, got %#v", history)
	}
}

func TestPreviousBranchStepHistoryIgnoresOtherBranchesAndRepos(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-scope", "https://example.com/repo.git", "main")
	other, _ := d.InsertRepo("/tmp/branch-history-other", "https://example.com/other.git", "main")
	insertRunWithReviewFindings(t, d, repo.ID, "other-branch", `{"findings":[{"id":"x","severity":"error","description":"x"}]}`)
	insertRunWithReviewFindings(t, d, other.ID, "feature", `{"findings":[{"id":"y","severity":"error","description":"y"}]}`)
	current, _ := d.InsertRun(repo.ID, "feature", "head", "base")

	history, err := d.PreviousBranchStepHistory(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step history: %v", err)
	}
	if history != nil {
		t.Fatalf("history leaked across branch or repo, got %#v", history)
	}
}

// A run whose review produced no findings carries no reusable history, so the
// lookup must fall through to the most recent run that actually reviewed
// something rather than reporting an empty history.
func TestPreviousBranchStepHistorySkipsRunsWithoutRecordedFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-empty", "https://example.com/repo.git", "main")
	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"real-1","severity":"warning","description":"real"}]}`)

	barren, _ := d.InsertRun(repo.ID, "feature", "head-barren", "base")
	barrenStep, _ := d.InsertStepResult(barren.ID, types.StepReview)
	if _, err := d.InsertStepRound(barrenStep.ID, 1, "initial", nil, nil, 5); err != nil {
		t.Fatalf("insert barren round: %v", err)
	}

	current, _ := d.InsertRun(repo.ID, "feature", "head-current", "base")
	history, err := d.PreviousBranchStepHistory(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step history: %v", err)
	}
	if history == nil || len(history.Rounds) != 1 || history.Rounds[0].FindingsJSON == nil {
		t.Fatalf("want the last run with recorded findings, got %#v", history)
	}
	if *history.Rounds[0].FindingsJSON != `{"findings":[{"id":"real-1","severity":"warning","description":"real"}]}` {
		t.Fatalf("wrong run selected: %s", *history.Rounds[0].FindingsJSON)
	}
}

func insertRunWithReviewFindings(t *testing.T, d *DB, repoID, branch, findingsJSON string) string {
	t.Helper()
	run, err := d.InsertRun(repoID, branch, "head-"+findingsJSON[:8], "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findingsJSON, nil, 10); err != nil {
		t.Fatalf("insert round: %v", err)
	}
	return run.ID
}
