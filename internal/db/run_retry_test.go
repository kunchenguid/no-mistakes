package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestACarriedStepNamesTheRowThatOwnsItsRounds(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	first, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	original, err := d.InsertStepResult(first.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertCarriedStepResult(first.ID, original); err == nil {
		t.Fatal("a pending step was carried")
	}
	if err := d.CompleteStepWithStatus(original.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	original, _ = d.GetStepResult(original.ID)

	second, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	carried, err := d.InsertCarriedStepResult(second.ID, original)
	if err != nil {
		t.Fatal(err)
	}
	third, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	again, err := d.InsertCarriedStepResult(third.ID, carried)
	if err != nil {
		t.Fatal(err)
	}
	read, err := d.GetStepResult(again.ID)
	if err != nil || read.Status != types.StepStatusCompleted || read.RoundsStepID() != original.ID || read.FindingsJSON != nil {
		t.Fatalf("twice-carried step = %+v (err %v), want completed, without findings, naming %s", read, err, original.ID)
	}

	if err := d.ResetStepsFrom(third.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	if read, _ := d.GetStepResult(again.ID); read.CarriedFrom != nil || read.RoundsStepID() != again.ID {
		t.Fatalf("revalidated step still reads another run's rounds: %+v", read)
	}
}

func TestFailStepWithCauseIsClearedByAPlainFailure(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.FailStepWithCause(step.ID, "agent exited", 1, StepFailureCauseAgentInvocation); err != nil {
		t.Fatal(err)
	}
	if read, _ := d.GetStepResult(step.ID); read.FailureCause == nil || *read.FailureCause != StepFailureCauseAgentInvocation {
		t.Fatalf("cause = %v, want %s", read.FailureCause, StepFailureCauseAgentInvocation)
	}
	if err := d.FailStep(step.ID, "aborted by user", 1); err != nil {
		t.Fatal(err)
	}
	if read, _ := d.GetStepResult(step.ID); read.FailureCause != nil {
		t.Fatalf("cause = %s survived a later failure for another reason", *read.FailureCause)
	}
}

func TestRunRetryLinksTheFailureAndItsContinuation(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	failed, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	continuation, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	link := RunRetry{RunID: continuation.ID, SourceRunID: failed.ID, StepName: types.StepReview, SourceRound: 4, SourceError: "agent exited", CheckpointHeadSHA: "head"}
	if err := d.InsertRunRetry(link); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRunRetry(continuation.ID)
	if err != nil || got == nil || got.SourceRunID != failed.ID || got.StepName != types.StepReview || got.SourceRound != 4 || got.SourceError != "agent exited" || got.CheckpointHeadSHA != "head" {
		t.Fatalf("link = %+v (err %v)", got, err)
	}
	if bySource, err := d.GetRunRetryBySource(failed.ID); err != nil || bySource == nil || bySource.RunID != continuation.ID {
		t.Fatalf("link by source = %+v (err %v)", bySource, err)
	}
	if none, err := d.GetRunRetry(failed.ID); err != nil || none != nil {
		t.Fatalf("a run that continues nothing has link %+v (err %v)", none, err)
	}
	other, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	link.RunID = other.ID
	if err := d.InsertRunRetry(link); err == nil {
		t.Fatal("a failed run was continued twice")
	}
}

func TestPreviousRunReviewRoundsFollowACarriedRowToItsOrigin(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	first, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	original, err := d.InsertStepResult(first.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(original.ID, 1, "initial", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStepWithStatus(original.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	original, _ = d.GetStepResult(original.ID)
	second, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if _, err := d.InsertCarriedStepResult(second.ID, original); err != nil {
		t.Fatal(err)
	}

	previous, err := d.GetPreviousRunReviewRounds(repo.ID, "feature", "superseding-run")
	if err != nil || previous == nil || previous.RunID != second.ID || len(previous.Rounds) != 1 || previous.Rounds[0].StepResultID != original.ID {
		t.Fatalf("previous = %+v (err %v), want the carried row's rounds from %s", previous, err, original.ID)
	}
}
