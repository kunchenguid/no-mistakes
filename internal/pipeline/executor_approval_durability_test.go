package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorApprovalGateParkFailureNeverPublishesTornGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { faultDB.Close() })
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_approval_step_park
		BEFORE UPDATE OF status ON step_results
		WHEN NEW.status = 'awaiting_approval'
		BEGIN
			SELECT RAISE(FAIL, 'injected approval step park failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	step := newApprovalStep(types.StepReview, `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`)
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = executor.Execute(ctx, run, repo, t.TempDir())
	if err == nil {
		t.Fatal("Execute accepted an approval gate whose atomic park write failed")
	}
	persistedRun, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persistedRun.AwaitingAgentSince != nil {
		t.Fatalf("failed gate left awaiting marker %d", *persistedRun.AwaitingAgentSince)
	}
	steps, getErr := database.GetStepsByRun(run.ID)
	if getErr != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, err = %v", steps, getErr)
	}
	if steps[0].Status == types.StepStatusAwaitingApproval {
		t.Fatal("failed gate exposed awaiting_approval without a complete transaction")
	}
}

func TestExecutorRespondJournalsBeforeAcknowledging(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`)
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_approval_response
		BEFORE INSERT ON approval_actions
		BEGIN
			SELECT RAISE(FAIL, 'injected approval response failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
		t.Fatal("Respond acknowledged an action that was not durable")
	}
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("failed response cleared the parked marker")
	}
	if _, err := faultDB.Exec(`DROP TRIGGER reject_approval_response`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("durable retry: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not consume durable response")
	}
}

func TestExecutorResumeReplaysPersistedApprovalActions(t *testing.T) {
	for _, tc := range []struct {
		action   types.ApprovalAction
		wantStep types.StepStatus
		wantRun  types.RunStatus
		wantErr  bool
	}{
		{action: types.ActionApprove, wantStep: types.StepStatusCompleted, wantRun: types.RunCompleted},
		{action: types.ActionSkip, wantStep: types.StepStatusSkipped, wantRun: types.RunCompleted},
		{action: types.ActionAbort, wantStep: types.StepStatusFailed, wantRun: types.RunFailed, wantErr: true},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`
			round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 17)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
				RunID: run.ID, StepResultID: step.ID, SourceRoundID: round.ID,
				Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 17,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertApprovalAction(db.ApprovalActionInput{
				GateID: gate.ID, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID,
				Action: tc.action, SelectedFindingIDsJSON: "null", InstructionsJSON: "null", AddedFindingsJSON: "null",
			}); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
			err = executor.Resume(context.Background(), run, repo, t.TempDir())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Resume() error = %v, wantErr %v", err, tc.wantErr)
			}
			persistedStep, getErr := database.GetStepResult(step.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			persistedRun, getErr := database.GetRun(run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if persistedStep.Status != tc.wantStep || persistedRun.Status != tc.wantRun || persistedRun.AwaitingAgentSince != nil {
				t.Fatalf("replayed %s = step %s run %s parked %v, want step %s run %s unparked", tc.action, persistedStep.Status, persistedRun.Status, persistedRun.AwaitingAgentSince, tc.wantStep, tc.wantRun)
			}
			pending, getErr := database.GetPendingApprovalAction(gate.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if pending != nil {
				t.Fatalf("replayed %s left pending action %+v", tc.action, pending)
			}
		})
	}
}

func approvalActionInput(gate *db.ApprovalGate, action types.ApprovalAction) db.ApprovalActionInput {
	return db.ApprovalActionInput{
		GateID: gate.ID, RunID: gate.RunID, StepResultID: gate.StepResultID, StepRoundID: gate.SourceRoundID,
		Action: action, SelectedFindingIDsJSON: "null", InstructionsJSON: "null", AddedFindingsJSON: "null",
	}
}

func waitForApprovalGate(t *testing.T, database *db.DB, stepResultID string) *db.ApprovalGate {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gate, err := database.GetCurrentApprovalGate(stepResultID)
		if err != nil {
			t.Fatal(err)
		}
		if gate != nil {
			return gate
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("approval gate for %s was never persisted", stepResultID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestExecutorResumeReplaysPersistedNonReviewFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"error","description":"tests require repair","action":"ask-user"}],"summary":"one"}`
	round, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: stepResult.ID, SourceRoundID: round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: stepResult.ID, StepRoundID: round.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["test-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	}); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	repairAgent := &scriptedExecutorRepairAgent{resolve: true}
	stepCalls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		stepCalls++
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(
		database,
		p,
		&config.Config{Routing: config.DefaultRoutingConfig()},
		repairAgent,
		[]Step{step},
		nil,
	)
	if err := executor.Resume(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Resume persisted fix: %v", err)
	}
	if stepCalls != 0 {
		t.Fatalf("replayed durable fix reran the original Test step %d time(s), want repair journal replay", stepCalls)
	}
	repairs, err := database.GetFindingRepairsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Status != db.RepairStatusResolved {
		t.Fatalf("replayed repair cycles = %+v, want one resolved cycle", repairs)
	}
	persistedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.Status != types.RunCompleted || persistedRun.AwaitingAgentSince != nil {
		t.Fatalf("replayed fix run = status %s parked %v", persistedRun.Status, persistedRun.AwaitingAgentSince)
	}
}

func TestExecutorNonReviewUserFixRequiresDurableVerifiedRepair(t *testing.T) {
	for _, stepName := range []types.StepName{types.StepTest, types.StepDocument, types.StepLint, types.StepVerify} {
		t.Run(string(stepName), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := gitInitTestDir(t)
			findingID := string(stepName) + "-1"
			findings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"error","description":%q,"action":"ask-user"}],"summary":"one"}`,
				findingID,
				string(stepName)+" requires a user-authorized repair",
			)
			step := newApprovalStep(stepName, findings)
			repairAgent := &scriptedExecutorRepairAgent{resolve: true}
			executor := NewExecutor(
				database,
				p,
				&config.Config{Routing: config.DefaultRoutingConfig()},
				repairAgent,
				[]Step{step},
				nil,
			)
			done := make(chan error, 1)
			go func() { done <- executor.Execute(context.Background(), run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, stepName, types.StepStatusAwaitingApproval)
			if err := executor.Respond(stepName, types.ActionFix, []string{findingID}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("verified user fix timed out")
			}
			repairs, err := database.GetFindingRepairsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(repairs) == 0 {
				t.Fatal("user fix completed without a durable repair cycle")
			}
			for _, repair := range repairs {
				if repair.Status != db.RepairStatusResolved ||
					repair.Verdict != db.RepairVerdictResolved ||
					repair.FixerAttemptID == "" ||
					repair.VerifierAttemptID == "" {
					t.Fatalf("repair = %+v, want independently verified resolved cycle", repair)
				}
			}
			persistedRun, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			unresolved, err := database.HasUnresolvedBlockingRepair(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persistedRun.Status != types.RunCompleted || unresolved {
				t.Fatalf("completed user fix = run %s unresolved %v", persistedRun.Status, unresolved)
			}
		})
	}
}
