package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ApprovalFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Step that needs approval on first call, passes on second
	callCount := 0
	var step Step = &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"issues":["bug"]}`}, nil
			}
			// After fix, re-evaluate passes
			return &StepOutcome{NeedsApproval: false, ExitCode: 0}, nil
		},
	}

	steps := []Step{step, newPassStep(types.StepTest)}
	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Wait for awaiting_approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Send fix action
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// Wait for step to re-execute and complete (it passes on second call)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Both steps should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("review: expected %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Errorf("test: expected %q, got %q", types.StepStatusCompleted, dbSteps[1].Status)
	}

	// Step should have been called twice (initial + after fix)
	if callCount != 2 {
		t.Errorf("expected step to be called 2 times, got %d", callCount)
	}
}

func TestExecutor_AwaitingAgentMarkerSetOnGateClearedOnRespond(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Entering the gate flips the pollable parked marker on.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run while parked: %v", err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil while parked at gate, want a timestamp")
	}

	// Responding clears it as the run resumes, so the marker is non-nil only
	// while the run is actually parked awaiting the agent.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	resumed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run after respond: %v", err)
	}
	if resumed.AwaitingAgentSince != nil {
		t.Errorf("AwaitingAgentSince = %d after respond, want nil", *resumed.AwaitingAgentSince)
	}
}

func TestExecutor_ResumeRestoresParkedGateAndReviewSessions(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs a fix","action":"ask-user"}],"summary":"one issue"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleReviewer), "fake", "reviewer-session"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "fake", "fixer-session"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	fake := newFakeSessionAgent()
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return nil, fmt.Errorf("recovered gate must not rerun its completed review pass")
			}
			if _, err := sctx.RunAgentSession(SessionRoleFixer, agent.RunOpts{Prompt: "fix"}); err != nil {
				return nil, err
			}
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Prompt: "rereview"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{SessionReuse: true}, fake, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never accepted a response: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	if len(fake.calls) != 2 {
		t.Fatalf("agent invocations = %d, want fixer and rereviewer", len(fake.calls))
	}
	if fake.calls[0].session == nil || fake.calls[0].session.ID != "fixer-session" {
		t.Fatalf("fixer session = %+v, want fixer-session", fake.calls[0].session)
	}
	if fake.calls[1].session == nil || fake.calls[1].session.ID != "reviewer-session" {
		t.Fatalf("reviewer session = %+v, want reviewer-session", fake.calls[1].session)
	}
	resumed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != types.RunCompleted || resumed.AwaitingAgentSince != nil {
		t.Fatalf("recovered run = status %s awaiting %v, want completed and unparked", resumed.Status, resumed.AwaitingAgentSince)
	}
}

func TestExecutorRecoveredGateUsesReviewSourceRoundAfterRepairFindings(t *testing.T) {
	database, p, run, _ := setupTest(t)
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
	initialFindings := `{"findings":[{"id":"review-1","severity":"warning","description":"initial finding","action":"auto-fix"}],"summary":"initial"}`
	currentFindings := `{"findings":[{"id":"verifier-1","severity":"warning","description":"needs consent","action":"ask-user"}],"summary":"verifier finding"}`
	if err := database.SetStepFindings(step.ID, currentFindings); err != nil {
		t.Fatal(err)
	}
	source, err := database.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose: types.PurposeInitialReview,
		Role:    types.InvocationRoleVerifier,
		Scope: types.InvocationScope{
			Kind:         types.InvocationScopePipeline,
			RunID:        run.ID,
			StepResultID: step.ID,
			StepRoundID:  source.ID,
		},
		CandidateKey: "review_strong:0:codex",
		Candidate: types.InvocationCandidate{
			Profile: "review_strong",
			Runner:  types.RunnerCodex,
			Model:   "test",
			Effort:  types.EffortMedium,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FinishInvocationAttempt(attempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteReservedStepRound(source.ID, &initialFindings, nil, 10); err != nil {
		t.Fatal(err)
	}
	repair, err := database.ReserveStepRound(step.ID, 2, "auto_fix")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteReservedStepRound(repair.ID, &currentFindings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	gate, err := executor.recoveredGate(run.ID)
	if err != nil {
		t.Fatalf("recoveredGate: %v", err)
	}
	if gate.lastRoundID != source.ID {
		t.Fatalf("recovered source round = %s, want original review round %s", gate.lastRoundID, source.ID)
	}
	if gate.round != repair.Round {
		t.Fatalf("recovered highest round = %d, want repair round %d", gate.round, repair.Round)
	}
}

func TestExecutor_ResumeRestoresNonReviewCompositeFindingGate(t *testing.T) {
	for _, stepName := range []types.StepName{types.StepTest, types.StepLint, types.StepDocument, types.StepVerify} {
		t.Run(string(stepName), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, stepName)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}

			originalID := string(stepName) + "-original"
			originalDescription := string(stepName) + " still fails after automatic repair"
			initialFindings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"error","description":%q,"action":"auto-fix"}],"summary":"initial failure"}`,
				originalID,
				originalDescription,
			)
			if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &initialFindings, nil, 10); err != nil {
				t.Fatal(err)
			}

			repairRound, err := database.ReserveStepRound(stepResult.ID, 2, "auto_fix")
			if err != nil {
				t.Fatal(err)
			}
			verifierAttempt, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
				Purpose: types.PurposeNormalAggregateVerification,
				Role:    types.InvocationRoleVerifier,
				Scope: types.InvocationScope{
					Kind:         types.InvocationScopePipeline,
					RunID:        run.ID,
					StepResultID: stepResult.ID,
					StepRoundID:  repairRound.ID,
				},
				CandidateKey: "review_strong:0:codex",
				Candidate: types.InvocationCandidate{
					Profile: "review_strong",
					Runner:  types.RunnerCodex,
					Model:   "test",
					Effort:  types.EffortMedium,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.FinishInvocationAttempt(verifierAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
				t.Fatal(err)
			}
			lineages, err := database.CreateFindingLineages(run.ID, verifierAttempt, []string{""})
			if err != nil {
				t.Fatal(err)
			}
			if len(lineages) != 1 {
				t.Fatalf("verifier-created lineages = %d, want 1", len(lineages))
			}
			consentID := lineages[0].DisplayID
			consentDescription := string(stepName) + " repair needs human judgment"
			repairFindings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"warning","description":%q,"action":"ask-user"}],"summary":"verifier-created finding"}`,
				consentID,
				consentDescription,
			)
			if err := database.CompleteReservedStepRound(repairRound.ID, &repairFindings, nil, 10); err != nil {
				t.Fatal(err)
			}
			originalRepair, err := database.StartFindingRepair(db.FindingRepairStart{
				RunID: run.ID, LineageID: "det:" + string(stepName) + ":" + run.ID + ":blocking:0",
				StepResultID: stepResult.ID, StepRoundID: repairRound.ID,
				Severity: "error", Action: types.ActionAutoFix, Description: originalDescription,
				Tier: 0, RemainingBudget: 0,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ResolveFindingRepair(originalRepair, db.RepairVerdictUnresolved, "still failing", db.RepairStatusUnresolved); err != nil {
				t.Fatal(err)
			}
			consentRepair, err := database.StartFindingRepair(db.FindingRepairStart{
				RunID: run.ID, LineageID: lineages[0].ID,
				StepResultID: stepResult.ID, StepRoundID: repairRound.ID,
				Severity: "warning", Action: types.ActionAskUser, Description: consentDescription,
				Tier: 0, RemainingBudget: 0,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ResolveFindingRepair(consentRepair, db.RepairVerdictUnresolved, "requires consent", db.RepairStatusUnresolved); err != nil {
				t.Fatal(err)
			}

			compositeFindings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"error","description":%q,"action":"auto-fix"},{"id":%q,"severity":"warning","description":%q,"action":"ask-user"}],"summary":"unresolved original and verifier-created consent gate"}`,
				originalID,
				originalDescription,
				consentID,
				consentDescription,
			)
			if err := database.SetStepFindings(stepResult.ID, compositeFindings); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 20); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(stepName)}, nil)
			gate, err := executor.recoveredGate(run.ID)
			if err != nil {
				t.Fatalf("recover composite gate: %v", err)
			}
			if gate.lastRoundID != repairRound.ID {
				t.Fatalf("recovered source round = %s, want producing repair round %s", gate.lastRoundID, repairRound.ID)
			}

			workDir := t.TempDir()
			initGitRepo(t, workDir)
			done := make(chan error, 1)
			go func() {
				done <- executor.Resume(context.Background(), run, repo, workDir)
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := executor.Respond(stepName, types.ActionSkip, nil); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("recovered composite gate never accepted a response")
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("resume composite gate: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovered composite gate timed out")
			}
			resumed, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Status != types.RunCompleted || resumed.AwaitingAgentSince != nil {
				t.Fatalf("recovered run = status %s awaiting %v, want completed and unparked", resumed.Status, resumed.AwaitingAgentSince)
			}
		})
	}
}

func TestExecutorRecoveredNonReviewCompositeGateRejectsCorruptOrUnownedFindings(t *testing.T) {
	for _, test := range []struct {
		name     string
		findings string
	}{
		{name: "corrupt", findings: `{"findings":[`},
		{name: "unowned", findings: `{"findings":[{"id":"test-original","severity":"error","description":"test failed","action":"auto-fix"},{"id":"intruder","severity":"warning","description":"not produced by any round","action":"ask-user"}],"summary":"contains unowned finding"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, p, run, _ := setupTest(t)
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
			initialFindings := `{"findings":[{"id":"test-original","severity":"error","description":"test failed","action":"auto-fix"}],"summary":"initial failure"}`
			if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &initialFindings, nil, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(stepResult.ID, test.findings); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}

			executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepTest)}, nil)
			if _, err := executor.recoveredGate(run.ID); err == nil {
				t.Fatal("recoveredGate accepted corrupt or unowned findings")
			}
		})
	}
}

func TestExecutorRecoveredReviewGateRejectsCorruptOrUnownedFindings(t *testing.T) {
	for _, test := range []struct {
		name     string
		findings string
	}{
		{name: "corrupt", findings: `{"findings":[`},
		{name: "unowned", findings: `{"findings":[{"id":"review-original","severity":"error","description":"review failed","action":"auto-fix"},{"id":"intruder","severity":"warning","description":"not produced by any round","action":"ask-user"}],"summary":"contains unowned finding"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, p, run, _ := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			initialFindings := `{"findings":[{"id":"review-original","severity":"error","description":"review failed","action":"auto-fix"}],"summary":"initial failure"}`
			round, err := database.ReserveStepRound(stepResult.ID, 1, "initial")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
				Purpose: types.PurposeInitialReview,
				Role:    types.InvocationRoleVerifier,
				Scope: types.InvocationScope{
					Kind:         types.InvocationScopePipeline,
					RunID:        run.ID,
					StepResultID: stepResult.ID,
					StepRoundID:  round.ID,
				},
				CandidateKey: "review_strong:0:codex",
				Candidate: types.InvocationCandidate{
					Profile: "review_strong",
					Runner:  types.RunnerCodex,
					Model:   "test",
					Effort:  types.EffortMedium,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.FinishInvocationAttempt(attempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteReservedStepRound(round.ID, &initialFindings, nil, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(stepResult.ID, test.findings); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}

			executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
			if _, err := executor.recoveredGate(run.ID); err == nil {
				t.Fatal("recoveredGate accepted corrupt or unowned review findings")
			}
		})
	}
}

func TestExecutor_ResumeRejectsApprovalWithUnresolvedRepair(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"unresolved","action":"ask-user"}],"summary":"one issue"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartFindingRepair(db.FindingRepairStart{
		RunID: run.ID, LineageID: "review-1", StepResultID: stepResult.ID, StepRoundID: round.ID,
		Severity: "error", Action: types.ActionAskUser, Description: "unresolved", Tier: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cannot be approved") {
			t.Fatalf("resume error = %v, want unresolved repair rejection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}
	persisted, err := database.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != types.StepStatusFailed || persisted.Error == nil || !strings.Contains(*persisted.Error, "cannot be approved") {
		t.Fatalf("recovered rejected step = status %s error %v, want durable failed state", persisted.Status, persisted.Error)
	}
}

func TestExecutor_TracksApprovalAndUserFixTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"error","description":"bug one","action":"auto-fix"},{"severity":"warn","description":"bug two","action":"ask-user"}],"summary":"2 issues"}`}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatalf("respond error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	approvalEvent := recorder.find("approval", "action", "fix")
	if approvalEvent == nil {
		t.Fatal("expected approval telemetry event")
	}
	if got := approvalEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("approval step = %v, want %q", got, types.StepReview)
	}
	if got := approvalEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("approval selected_findings_count = %v, want 2", got)
	}

	fixEvent := recorder.find("fix", "source", "user")
	if fixEvent == nil {
		t.Fatal("expected user fix telemetry event")
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("fix selected_findings_count = %v, want 2", got)
	}

	stepEvent := recorder.find("step", "status", string(types.StepStatusAwaitingApproval))
	if stepEvent == nil {
		t.Fatal("expected awaiting approval step telemetry event")
	}
	if got := stepEvent.fields["findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("step findings_count = %v, want 2", got)
	}
}
