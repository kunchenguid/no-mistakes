package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type continuingApprovalStep struct {
	name      types.StepName
	executes  atomic.Int32
	continues atomic.Bool
}

func (s *continuingApprovalStep) Name() types.StepName { return s.name }

func (s *continuingApprovalStep) Execute(*StepContext) (*StepOutcome, error) {
	n := s.executes.Add(1)
	if n == 1 {
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"b1","severity":"warning","description":"bot","action":"ask-user"}],"summary":"early"}`}, nil
	}
	return &StepOutcome{}, nil
}

func (s *continuingApprovalStep) ContinueAfterApproval(*StepContext, string) bool {
	return s.continues.Load()
}

func TestExecutor_ApprovalContinuerReentersTheStepInsteadOfCompleting(t *testing.T) {
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
		t.Run(string(action), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &continuingApprovalStep{name: types.StepCI}
			step.continues.Store(true)
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

			done := make(chan error, 1)
			go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			if err := exec.Respond(types.StepCI, action, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("run did not finish")
			}
			if got := step.executes.Load(); got != 2 {
				t.Fatalf("step executed %d times, want the park plus one monitoring re-entry", got)
			}
		})
	}
}

func TestExecutor_ApprovalWithoutContinuationCompletesTheStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &continuingApprovalStep{name: types.StepCI}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := step.executes.Load(); got != 1 {
		t.Fatalf("step executed %d times, want 1", got)
	}
}

type reparkingApprovalStep struct {
	continuingApprovalStep
}

func (s *reparkingApprovalStep) Execute(*StepContext) (*StepOutcome, error) {
	if s.executes.Add(1) <= 2 {
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"c1","severity":"error","description":"red check","action":"ask-user"}],"summary":"parked"}`}, nil
	}
	return &StepOutcome{}, nil
}

func TestExecutor_RecoveredApprovalContinuerReentersAndParksAgain(t *testing.T) {
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
		t.Run(string(action), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"b1","severity":"warning","description":"bot","action":"ask-user"}],"summary":"early"}`
			if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 10); err != nil {
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

			step := &reparkingApprovalStep{continuingApprovalStep{name: types.StepCI}}
			step.continues.Store(true)
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done := make(chan error, 1)
			go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

			respond := func(act types.ApprovalAction) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				var respondErr error
				for time.Now().Before(deadline) {
					if respondErr = exec.Respond(types.StepCI, act, nil); respondErr == nil {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatalf("Respond(%s) error = %v", act, respondErr)
			}
			respond(action)
			deadline := time.Now().Add(5 * time.Second)
			for step.executes.Load() < 1 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			step.continues.Store(false)
			respond(types.ActionApprove)

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Resume() error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovered run did not finish")
			}
			if got := step.executes.Load(); got != 1 {
				t.Fatalf("step executed %d times, want one monitoring re-entry that parks again", got)
			}
			rounds, err := database.GetRoundsByStep(stepResult.ID)
			if err != nil {
				t.Fatal(err)
			}
			var triggers []string
			for _, r := range rounds {
				triggers = append(triggers, r.Trigger)
			}
			if len(triggers) < 2 || triggers[1] != "monitor" {
				t.Fatalf("round triggers = %v, want the round after the recovered approval recorded as monitor", triggers)
			}
		})
	}
}
