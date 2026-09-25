package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGetBranchTestEvidence_ScopesToOtherRunsOnTheSameBranch: a test verdict
// is evidence about one branch's head, so the reuse lookup must never see
// another branch's or another repository's runs, nor the caller's own run.
func TestGetBranchTestEvidence_ScopesToOtherRunsOnTheSameBranch(t *testing.T) {
	d := openTestDB(t)
	repoA, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := d.InsertRepo(t.TempDir(), "https://example.invalid/b", "main")
	if err != nil {
		t.Fatal(err)
	}

	seed := func(repoID, branch string, step types.StepName, status types.StepStatus, findings *string) *Run {
		run, err := d.InsertRun(repoID, branch, "head-"+branch, "base")
		if err != nil {
			t.Fatal(err)
		}
		sr, err := d.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if findings != nil {
			if err := d.SetStepFindings(sr.ID, *findings); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.CompleteStepWithStatus(sr.ID, status, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		return run
	}

	payload := `{"findings":[],"summary":"","verdict":"go","tested_head_sha":"abc123"}`
	wanted := seed(repoA.ID, "feature", types.StepTest, types.StepStatusCompleted, &payload)
	current := seed(repoA.ID, "feature", types.StepTest, types.StepStatusCompleted, &payload)
	seed(repoA.ID, "other", types.StepTest, types.StepStatusCompleted, &payload)
	seed(repoB.ID, "feature", types.StepTest, types.StepStatusCompleted, &payload)
	// A different step, and a step whose findings a fix round cleared, both
	// have nothing to offer.
	seed(repoA.ID, "feature", types.StepReview, types.StepStatusCompleted, &payload)
	seed(repoA.ID, "feature", types.StepTest, types.StepStatusCompleted, nil)

	got, err := d.GetBranchTestEvidence(repoA.ID, "feature", current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entry = nil, want the other same-branch test step")
	}
	if got.RunID != wanted.ID || got.FindingsJSON != payload {
		t.Fatalf("entry = %+v, want run %s with its recorded findings", got, wanted.ID)
	}
}

// Only the most recently recorded evidence is returned at all: an older
// verdict a later run has already superseded must never outrank it.
func TestGetBranchTestEvidence_ReturnsOnlyTheNewestRecordedVerdict(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, head := range []string{"one", "two", "three"} {
		run, err := d.InsertRun(repo.ID, "feature", head, "base")
		if err != nil {
			t.Fatal(err)
		}
		sr, err := d.InsertStepResult(run.ID, types.StepTest)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetStepFindings(sr.ID, `{"findings":[],"summary":"","verdict":"go","tested_head_sha":"`+head+`"}`); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteStep(sr.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}

	got, err := d.GetBranchTestEvidence(repo.ID, "feature", "none")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entry = nil, want the most recently recorded run")
	}
	if got.RunID != ids[2] {
		t.Fatalf("entry = %s, want only the most recently recorded run %s", got.RunID, ids[2])
	}
}

// The reuse decision compares the intent a verdict was earned under, so the
// query must carry it. runs.intent is nullable and most runs carry none, which
// must read as an empty intent rather than a scan error - an error there would
// be logged and silently decline reuse forever.
func TestGetBranchTestEvidence_CarriesTheRunsIntentAndToleratesItsAbsence(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(branch, intent string) {
		run, err := d.InsertRun(repo.ID, branch, "head-"+branch, "base")
		if err != nil {
			t.Fatal(err)
		}
		if intent != "" {
			if err := d.UpdateRunIntent(run.ID, RunIntent{Summary: intent, Source: "user", SessionID: "s", Score: 1}); err != nil {
				t.Fatal(err)
			}
		}
		sr, err := d.InsertStepResult(run.ID, types.StepTest)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetStepFindings(sr.ID, `{"findings":[],"summary":"","verdict":"go","tested_head_sha":"abc123"}`); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteStep(sr.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	seed("with-intent", "ship the checkout success screen")
	seed("no-intent", "")

	got, err := d.GetBranchTestEvidence(repo.ID, "with-intent", "none")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Intent != "ship the checkout success screen" {
		t.Fatalf("entry = %+v, want the run's recorded intent", got)
	}

	got, err = d.GetBranchTestEvidence(repo.ID, "no-intent", "none")
	if err != nil {
		t.Fatalf("a run with no intent must not fail the query: %v", err)
	}
	if got == nil || got.Intent != "" {
		t.Fatalf("entry = %+v, want an empty intent", got)
	}
}

// Nothing recorded on the branch is not an error: the caller simply falls
// through to running the evidence agent.
func TestGetBranchTestEvidence_NoEvidenceIsNotAnError(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetBranchTestEvidence(repo.ID, "feature", "none")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("entry = %+v, want nil", got)
	}
}

// TestGetBranchTestEvidence_ParkedVerdictOutranksAnOlderCompletedOne: a Test
// step that recorded a verdict and then parked keeps its payload, and all
// three ways such a step ends non-completed (crash recovery failing it, an
// abort, a skip) keep it too. That verdict is still the branch's latest
// evidence, so hiding it behind an older completed verdict would let a
// superseded `go` be reused over a recorded `no-go`. The ordering is the other
// half of the same guarantee: a parked row's completed_at is NULL, which
// SQLite sorts LAST under DESC, so the row id carries recency instead.
func TestGetBranchTestEvidence_ParkedVerdictOutranksAnOlderCompletedOne(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	record := func(verdict string, park bool) string {
		run, err := d.InsertRun(repo.ID, "feature", "head-"+verdict, "base")
		if err != nil {
			t.Fatal(err)
		}
		sr, err := d.InsertStepResult(run.ID, types.StepTest)
		if err != nil {
			t.Fatal(err)
		}
		payload := `{"findings":[],"summary":"","verdict":"` + verdict + `","tested_head_sha":"abc123"}`
		if park {
			if err := d.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &payload); err != nil {
				t.Fatal(err)
			}
			return run.ID
		}
		if err := d.SetStepFindings(sr.ID, payload); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteStep(sr.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}

	record(types.TestVerdictGo, false)
	parked := record(types.TestVerdictNoGo, true)

	got, err := d.GetBranchTestEvidence(repo.ID, "feature", "none")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entry = nil, want the branch's newest recorded verdict")
	}
	if got.RunID != parked {
		t.Fatalf("entry = %s, want the parked no-go run %s: the newest recorded verdict wins whether or not its step completed", got.RunID, parked)
	}
}
