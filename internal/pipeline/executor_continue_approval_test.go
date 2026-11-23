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
