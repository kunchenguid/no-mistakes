package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_StampsPositionalStepOrder verifies step_order records the
// step's position in the run's pipeline (1-indexed slice order), so a custom
// steps: selection renders and sorts in the order it actually ran.
func TestExecutor_StampsPositionalStepOrder(t *testing.T) {
	database, p, run, repo := setupTest(t)

	stepNames := []types.StepName{types.StepRebase, types.StepTest, types.StepPush}
	steps := make([]Step, len(stepNames))
	for i, name := range stepNames {
		steps[i] = newPassStep(name)
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	records, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(records) != len(stepNames) {
		t.Fatalf("got %d step records, want %d", len(records), len(stepNames))
	}
	// GetStepsByRun sorts by step_order, so positional stamping also keeps
	// the read-back sequence equal to the executed sequence.
	for i, rec := range records {
		if rec.StepName != stepNames[i] {
			t.Errorf("records[%d] = %q, want %q", i, rec.StepName, stepNames[i])
		}
		if rec.StepOrder != i+1 {
			t.Errorf("step %q order = %d, want positional %d (not legacy ordinal %d)", rec.StepName, rec.StepOrder, i+1, rec.StepName.Order())
		}
	}
}

// TestExecutor_DefaultPipelineOrderMatchesLegacyOrdinals proves backward
// compatibility: for the default nine-step pipeline the positional order is
// byte-for-byte what the legacy fixed StepName.Order() ordinals produced.
func TestExecutor_DefaultPipelineOrderMatchesLegacyOrdinals(t *testing.T) {
	database, p, run, repo := setupTest(t)

	defaultNames := types.AllSteps()
	steps := make([]Step, len(defaultNames))
	for i, name := range defaultNames {
		steps[i] = newPassStep(name)
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	records, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(records) != len(defaultNames) {
		t.Fatalf("got %d step records, want %d", len(records), len(defaultNames))
	}
	for _, rec := range records {
		if rec.StepOrder != rec.StepName.Order() {
			t.Errorf("step %q order = %d, want legacy ordinal %d", rec.StepName, rec.StepOrder, rec.StepName.Order())
		}
	}
}
