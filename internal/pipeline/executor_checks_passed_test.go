package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type releasingCIStep struct{ declaredNoCI bool }

func (releasingCIStep) Name() types.StepName { return types.StepCI }

func (s releasingCIStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"); err != nil {
		return nil, err
	}
	declaredNoCI := s.declaredNoCI
	return &StepOutcome{CIReadyNoCI: &declaredNoCI}, nil
}

// ciStepSnapshot is the recorded CI step state a terminal run must keep: its
// status and the wall-clock duration the executor measured for it.
type ciStepState struct {
	status     string
	durationMS int64
}

func ciStepSnapshot(t *testing.T, database *db.DB, runID string) ciStepState {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepCI {
			continue
		}
		snapshot := ciStepState{status: string(step.Status)}
		if step.DurationMS != nil {
			snapshot.durationMS = *step.DurationMS
		}
		return snapshot
	}
	t.Fatalf("run %s has no CI step result", runID)
	return ciStepState{}
}

// TestExecutor_GreenCIRunRecordsChecksPassedAndReleases is the run-level half
// of the CI step's default verdict: a run whose checks came back green while
// its PR was still open finishes as checks_passed, not as an ordinary
// completion, so a caller can tell "checks passed, waiting on a human merge
// decision" apart from "the PR merged or closed" - and the merge is not this
// run's to observe.
//
// The second half proves the recorded outcome is stable and the duration is
// CI-only: the merge is observed afterwards (recorded as PR truth), and
// neither the run's status nor the CI step's status and duration move.
func TestExecutor_GreenCIRunRecordsChecksPassedAndReleases(t *testing.T) {
	database, p, run, repo := setupTest(t)
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{releasingCIStep{}}, events.handler)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunChecksPassed {
		t.Fatalf("run status = %s, want %s", got.Status, types.RunChecksPassed)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunChecksPassed) {
		t.Fatalf("terminal event = %+v, want a checks_passed run_completed", completed)
	}

	before := ciStepSnapshot(t, database, run.ID)
	if before.status != string(types.StepStatusCompleted) {
		t.Fatalf("CI step status = %s, want completed", before.status)
	}

	// A merge observed afterwards is PR truth, and must not rewrite what this
	// run recorded: its outcome and the recorded CI duration stay put.
	if err := database.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	if repaired, err := database.ReconcileTerminalPRRuns(); err != nil {
		t.Fatalf("ReconcileTerminalPRRuns() error = %v", err)
	} else if repaired != 0 {
		t.Fatalf("reconciled runs = %d, want none: a checks_passed run is already terminal", repaired)
	}

	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunChecksPassed {
		t.Fatalf("run status after the merge observation = %s, want %s", after.Status, types.RunChecksPassed)
	}
	if after.PRState == nil || *after.PRState != "merged" {
		t.Fatalf("PR state = %v, want the merge recorded as PR truth", after.PRState)
	}
	afterStep := ciStepSnapshot(t, database, run.ID)
	if afterStep.status != before.status || afterStep.durationMS != before.durationMS {
		t.Fatalf("CI step after the merge observation = %+v, want it unchanged at %+v", afterStep, before)
	}
}

// TestExecutor_MergedCIRunStillRecordsCompleted keeps the other outcome
// distinct: a run that ended because the PR merged records an ordinary
// completed run, whose agent-facing outcome is "passed", not "checks-passed".
func TestExecutor_MergedCIRunStillRecordsCompleted(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{mergedCIStep{}}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s for a merged PR", got.Status, types.RunCompleted)
	}
}

// mergedCIStep is the CI step's other terminal shape: the PR merged, so the run
// completes rather than releasing at a green head.
type mergedCIStep struct{ releasingCIStep }

func (mergedCIStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := sctx.DB.SetRunCIReadyWithReason(sctx.Run.ID, true, false); err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
		return nil, err
	}
	return &StepOutcome{}, nil
}

func TestExecutor_CIReadinessWriteFailureFailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	raw, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_readiness BEFORE UPDATE OF ci_ready_at ON runs WHEN NEW.ci_ready_at IS NOT NULL BEGIN SELECT RAISE(FAIL, 'injected readiness failure'); END`); err != nil {
		t.Fatal(err)
	}
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{releasingCIStep{}}, events.handler)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "injected readiness failure") {
		t.Fatalf("Execute = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CIReadyAt != nil {
		t.Fatal("failed completion persisted readiness")
	}
	if step := ciStepSnapshot(t, database, run.ID); step.status == string(types.StepStatusCompleted) {
		t.Fatal("readiness failure left CI completed")
	}
}

func TestExecutor_TerminalVerdictReadErrors(t *testing.T) {
	for _, target := range []string{"runs", "step_results"} {
		t.Run(target, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunPRState(run.ID, "open"); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunCIReady(run.ID, true); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec("ALTER TABLE " + target + " RENAME TO unavailable"); err != nil {
				t.Fatal(err)
			}
			events := &eventCollector{}
			exec := NewExecutor(database, p, nil, nil, nil, events.handler)
			if err := exec.completeRun(run, repo); err == nil {
				t.Fatal("unreadable verdict evidence reported success")
			}
			if events.findRunEvent(ipc.EventRunCompleted) != nil {
				t.Fatal("unreadable evidence emitted completion")
			}
		})
	}
}

func TestExecutor_CIReleaseReadinessFollowsStepCompletion(t *testing.T) {
	for _, declaredNoCI := range []bool{false, true} {
		t.Run(map[bool]string{false: "checks", true: "declared-no-ci"}[declaredNoCI], func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			completedEvent := false
			readyEvent := false
			exec := NewExecutor(database, p, nil, nil, []Step{releasingCIStep{declaredNoCI: declaredNoCI}}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepCI {
					completedEvent = true
				}
				if event.CIReady != nil && *event.CIReady {
					readyEvent = true
					if !completedEvent {
						t.Error("live readiness event preceded CI completion")
					}
					if event.CIReadyNoCI == nil || *event.CIReadyNoCI != declaredNoCI {
						t.Error("readiness event lost declaration")
					}
				}
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.CIReadyAt != nil && ciStepSnapshot(t, database, run.ID).status != string(types.StepStatusCompleted) {
					t.Error("persisted readiness exposed an active release step")
				}
			})
			if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if !readyEvent {
				t.Fatal("completed CI did not publish readiness")
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != types.RunChecksPassed || stored.CIReadyAt == nil || stored.CIReadyNoCI != declaredNoCI {
				t.Fatalf("lost durable release verdict: %+v", stored)
			}
		})
	}
}
