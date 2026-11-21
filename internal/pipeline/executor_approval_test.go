package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
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

func TestExecutor_ResumePromotesDurableReviewedCandidateOnApproval(t *testing.T) {
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
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"decision","action":"ask-user"}]}`
	const reviewedHead = "3333333333333333333333333333333333333333"
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, reviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, workDir) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered review never accepted approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != reviewedHead {
		t.Fatalf("recovered approval = %#v, want %s", got.ReviewApprovedHeadSHA, reviewedHead)
	}
}

func TestExecutor_CustomGateTelemetryRedactsLabel(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	stepName := types.CustomGateStepName(types.StepReview, "private-policy")

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: stepName,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"error","description":"bug one","action":"auto-fix"},{"severity":"warn","description":"bug two","action":"ask-user"}],"summary":"2 issues"}`}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentOMP}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, stepName, types.StepStatusAwaitingApproval)

	if err := respondFixPartial(t, exec, stepName); err != nil {
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
	if got := approvalEvent.fields["step"]; got != "gate" {
		t.Fatalf("approval step = %v, want gate", got)
	}
	if got := approvalEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("approval selected_findings_count = %v, want 2", got)
	}

	fixEvent := recorder.find("fix", "source", "user")
	if fixEvent == nil {
		t.Fatal("expected user fix telemetry event")
	}
	if got := fixEvent.fields["step"]; got != "gate" {
		t.Fatalf("fix step = %v, want gate", got)
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("fix selected_findings_count = %v, want 2", got)
	}

	stepEvent := recorder.find("step", "status", string(types.StepStatusAwaitingApproval))
	if stepEvent == nil {
		t.Fatal("expected awaiting approval step telemetry event")
	}
	if got := stepEvent.fields["step"]; got != "gate" {
		t.Fatalf("step telemetry step = %v, want gate", got)
	}
	if got := stepEvent.fields["findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("step findings_count = %v, want 2", got)
	}
	if got := stepEvent.fields["agent"]; got != string(types.AgentOMP) {
		t.Fatalf("step agent = %v, want %q", got, types.AgentOMP)
	}
}

func TestExecutor_TracksAutoFixTelemetry(t *testing.T) {
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
				return &StepOutcome{
					AutoFixable: true,
					Findings:    `{"findings":[{"severity":"error","file":"main.go","description":"fix me","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{ExitCode: 0, ReviewedPaths: []string{"main.go"}, ReviewablePaths: []string{"main.go"}}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentOMP, AutoFix: config.AutoFix{Review: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	fixEvent := recorder.find("fix", "source", "auto")
	if fixEvent == nil {
		t.Fatal("expected auto-fix telemetry event")
	}
	if got := fixEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("fix step = %v, want %q", got, types.StepReview)
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix selected_findings_count = %v, want 1", got)
	}
	if got := fixEvent.fields["attempt"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix attempt = %v, want 1", got)
	}
}
