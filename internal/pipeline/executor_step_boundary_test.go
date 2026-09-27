package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// collectingStep runs nothing and records that the executor reached it, so a test
// can assert the boundary hook fired before it.
func collectingStep(name types.StepName, reached *[]types.StepName) Step {
	return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
		*reached = append(*reached, name)
		return &StepOutcome{}, nil
	}}
}

// TestExecutor_StepBoundaryFuncRunsOncePerExecutedStep pins the seam quota-auto
// re-selects through: one call per step, in order, for exactly the steps that run.
func TestExecutor_StepBoundaryFuncRunsOncePerExecutedStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	reached := make([]types.StepName, 0, 2)
	steps := []Step{
		collectingStep(types.StepReview, &reached),
		collectingStep(types.StepTest, &reached),
	}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, &usageAgent{}, steps, nil)
	var boundaries []types.StepName
	exec.SetStepBoundaryFunc(func(_ context.Context, step types.StepName) error {
		boundaries = append(boundaries, step)
		return nil
	})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(reached) != 2 {
		t.Fatalf("steps reached = %v", reached)
	}
	// The hook fires before its step, which is what makes it a boundary rather
	// than a post-mortem.
	if len(boundaries) != 2 || boundaries[0] != types.StepReview || boundaries[1] != types.StepTest {
		t.Fatalf("boundaries = %v, want one per step in order", boundaries)
	}
}

// TestExecutor_StepBoundaryFuncSkipsSkippedSteps proves the hook is not asked to
// route a step that never runs.
func TestExecutor_StepBoundaryFuncSkipsSkippedSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	reached := make([]types.StepName, 0, 2)
	steps := []Step{
		collectingStep(types.StepReview, &reached),
		collectingStep(types.StepTest, &reached),
	}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, &usageAgent{}, steps, nil)
	exec.SetSkippedSteps([]types.StepName{types.StepTest})
	var boundaries []types.StepName
	exec.SetStepBoundaryFunc(func(_ context.Context, step types.StepName) error {
		boundaries = append(boundaries, step)
		return nil
	})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(boundaries) != 1 || boundaries[0] != types.StepReview {
		t.Fatalf("boundaries = %v, want only the executed step", boundaries)
	}
	if len(reached) != 1 || reached[0] != types.StepReview {
		t.Fatalf("steps reached = %v", reached)
	}
}

// TestExecutor_StepBoundaryFuncFailsTheRun proves a boundary that cannot route
// anywhere ends the run with its own report instead of starting a step on a
// harness the evidence refused.
func TestExecutor_StepBoundaryFuncFailsTheRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	reached := make([]types.StepName, 0, 1)
	steps := []Step{collectingStep(types.StepReview, &reached)}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, &usageAgent{}, steps, nil)
	exec.SetStepBoundaryFunc(func(context.Context, types.StepName) error {
		return errors.New("quota-auto found no eligible agent for step \"review\": claude: quota exhausted now")
	})

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no eligible agent") {
		t.Fatalf("error = %v, want the boundary report", err)
	}
	if len(reached) != 0 {
		t.Fatalf("steps reached = %v, want the step never started", reached)
	}
	stored, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatalf("get run: %v", getErr)
	}
	if stored == nil || stored.Status != types.RunFailed {
		t.Fatalf("run status = %v, want failed", stored)
	}
}

// TestExecutor_NoStepBoundaryFuncIsTodaysPipeline proves the hook is additive: a
// run without one behaves exactly as before.
func TestExecutor_NoStepBoundaryFuncIsTodaysPipeline(t *testing.T) {
	database, p, run, repo := setupTest(t)
	reached := make([]types.StepName, 0, 1)
	steps := []Step{collectingStep(types.StepReview, &reached)}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, &usageAgent{}, steps, nil)
	exec.SetStepBoundaryFunc(nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(reached) != 1 {
		t.Fatalf("steps reached = %v", reached)
	}
}
