package db

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testValidationCheckpoint(runID string) *ValidationCheckpoint {
	return &ValidationCheckpoint{
		RunID: runID, Version: 1,
		ValidatedSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		ConfigHash: strings.Repeat("c", 64), IntentHash: strings.Repeat("d", 64),
		EvidenceHashes: map[string]string{"artifact-manifest": strings.Repeat("e", 64)},
	}
}

func TestValidationCheckpointSourceDeletionKeepsAuditRow(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-delete", "https://example.com/repo.git", "main")
	source, _ := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	target, _ := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err := database.PutValidationCheckpoint(testValidationCheckpoint(source.ID)); err != nil {
		t.Fatal(err)
	}
	targetCheckpoint := testValidationCheckpoint(target.ID)
	targetCheckpoint.ReusedFromRunID = &source.ID
	if err := database.PutValidationCheckpoint(targetCheckpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`DELETE FROM runs WHERE id = ?`, source.ID); err != nil {
		t.Fatalf("delete reused source run: %v", err)
	}
	got, err := database.GetValidationCheckpoint(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ReusedFromRunID != nil {
		t.Fatalf("target checkpoint after source deletion = %#v", got)
	}
}

func TestFailRunAndInvalidateValidationCheckpointIsAtomic(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-fail", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err := database.PutValidationCheckpoint(testValidationCheckpoint(run.ID)); err != nil {
		t.Fatal(err)
	}
	if err := database.FailRunAndInvalidateValidationCheckpoint(run.ID, "dirty", types.RunFailed, nil); err != nil {
		t.Fatal(err)
	}
	gotRun, _ := database.GetRun(run.ID)
	gotCheckpoint, _ := database.GetValidationCheckpoint(run.ID)
	if gotRun.Status != types.RunFailed || gotCheckpoint != nil {
		t.Fatalf("terminal state = %s, checkpoint = %#v", gotRun.Status, gotCheckpoint)
	}
}

func TestFailActiveRecoveredRunPreservesConcurrentCancellation(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-cancel", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err := database.PutValidationCheckpoint(testValidationCheckpoint(run.ID)); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(run.ID, types.RunCancelReasonSuperseded, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	changed, err := database.FailActiveRecoveredRun(run.ID, "recovery failed", true)
	if err != nil {
		t.Fatal(err)
	}
	gotRun, _ := database.GetRun(run.ID)
	gotCheckpoint, _ := database.GetValidationCheckpoint(run.ID)
	if changed || gotRun.Status != types.RunCancelled || gotCheckpoint == nil {
		t.Fatalf("changed = %v, status = %s, checkpoint = %#v", changed, gotRun.Status, gotCheckpoint)
	}
}

func TestRecoverStaleRunsInvalidatesUnpreservedCheckpoints(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-stale", "https://example.com/repo.git", "main")
	preserved, _ := database.InsertRun(repo.ID, "preserved", strings.Repeat("a", 40), strings.Repeat("b", 40))
	stale, _ := database.InsertRun(repo.ID, "stale", strings.Repeat("a", 40), strings.Repeat("b", 40))
	for _, run := range []*Run{preserved, stale} {
		if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		if err := database.PutValidationCheckpoint(testValidationCheckpoint(run.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.RecoverStaleRunsExcept("crashed", map[string]struct{}{preserved.ID: {}}); err != nil {
		t.Fatal(err)
	}
	kept, _ := database.GetValidationCheckpoint(preserved.ID)
	removed, _ := database.GetValidationCheckpoint(stale.ID)
	if kept == nil || removed != nil {
		t.Fatalf("preserved checkpoint = %#v, stale checkpoint = %#v", kept, removed)
	}
}

func TestRearmDeliveryPreservesTerminalRunState(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-rearm-terminal", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "branch", "head", "base")
	for _, name := range types.AllSteps() {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name.Order() < types.StepPush.Order() {
			if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RearmDeliveryAfterCrash(run.ID); err == nil {
		t.Fatal("terminal run was rearmed")
	}
	got, _ := database.GetRun(run.ID)
	if got.Status != types.RunCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
}

func TestRecoverStaleRunsPreservesRecognizedCICheckpoint(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo("/tmp/checkpoint-ci-stale", "https://example.com/repo.git", "main")
	run, _ := database.InsertRun(repo.ID, "branch", "head", "base")
	if err := database.PutValidationCheckpoint(testValidationCheckpoint(run.ID)); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecoverStaleRunsExceptWithCheckpoints("crashed", nil, map[string]struct{}{run.ID: {}}); err != nil {
		t.Fatal(err)
	}
	gotRun, _ := database.GetRun(run.ID)
	gotCheckpoint, _ := database.GetValidationCheckpoint(run.ID)
	if gotRun.Status != types.RunFailed || gotCheckpoint == nil {
		t.Fatalf("status = %s, checkpoint = %#v", gotRun.Status, gotCheckpoint)
	}
}
