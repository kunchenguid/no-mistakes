package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPreviousBranchStepRoundsReturnsMostRecentPriorRunOnSameBranch(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history", "https://example.com/repo.git", "main")

	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"older-1","severity":"warning","description":"older"}]}`)
	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"newer-1","severity":"error","description":"newer"}]}`)
	current, _ := d.InsertRun(repo.ID, "feature", "head3", "base")

	rounds, err := d.PreviousBranchStepRounds(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step rounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("want rounds from exactly the newest prior run, got %d", len(rounds))
	}
	if *rounds[0].FindingsJSON != `{"findings":[{"id":"newer-1","severity":"error","description":"newer"}]}` {
		t.Fatalf("returned rounds from the wrong run: %s", *rounds[0].FindingsJSON)
	}
}

func TestPreviousBranchStepRoundsExcludesCurrentRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-self", "https://example.com/repo.git", "main")
	current := insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"self-1","severity":"warning","description":"self"}]}`)

	rounds, err := d.PreviousBranchStepRounds(repo.ID, "feature", types.StepReview, current)
	if err != nil {
		t.Fatalf("previous branch step rounds: %v", err)
	}
	if len(rounds) != 0 {
		t.Fatalf("current run must never be its own prior history, got %d rounds", len(rounds))
	}
}

func TestPreviousBranchStepRoundsIgnoresOtherBranchesAndRepos(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-scope", "https://example.com/repo.git", "main")
	other, _ := d.InsertRepo("/tmp/branch-history-other", "https://example.com/other.git", "main")
	insertRunWithReviewFindings(t, d, repo.ID, "other-branch", `{"findings":[{"id":"x","severity":"error","description":"x"}]}`)
	insertRunWithReviewFindings(t, d, other.ID, "feature", `{"findings":[{"id":"y","severity":"error","description":"y"}]}`)
	current, _ := d.InsertRun(repo.ID, "feature", "head", "base")

	rounds, err := d.PreviousBranchStepRounds(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step rounds: %v", err)
	}
	if len(rounds) != 0 {
		t.Fatalf("history leaked across branch or repo, got %d rounds", len(rounds))
	}
}

// A run whose review produced no findings carries no reusable history, so the
// lookup must fall through to the most recent run that actually reviewed
// something rather than reporting an empty history.
func TestPreviousBranchStepRoundsSkipsRunsWithoutRecordedFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/branch-history-empty", "https://example.com/repo.git", "main")
	insertRunWithReviewFindings(t, d, repo.ID, "feature", `{"findings":[{"id":"real-1","severity":"warning","description":"real"}]}`)

	barren, _ := d.InsertRun(repo.ID, "feature", "head-barren", "base")
	barrenStep, _ := d.InsertStepResult(barren.ID, types.StepReview)
	if _, err := d.InsertStepRound(barrenStep.ID, 1, "initial", nil, nil, 5); err != nil {
		t.Fatalf("insert barren round: %v", err)
	}

	current, _ := d.InsertRun(repo.ID, "feature", "head-current", "base")
	rounds, err := d.PreviousBranchStepRounds(repo.ID, "feature", types.StepReview, current.ID)
	if err != nil {
		t.Fatalf("previous branch step rounds: %v", err)
	}
	if len(rounds) != 1 || rounds[0].FindingsJSON == nil {
		t.Fatalf("want the last run with recorded findings, got %#v", rounds)
	}
	if *rounds[0].FindingsJSON != `{"findings":[{"id":"real-1","severity":"warning","description":"real"}]}` {
		t.Fatalf("wrong run selected: %s", *rounds[0].FindingsJSON)
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
