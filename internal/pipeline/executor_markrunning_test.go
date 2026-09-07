package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A fix round is marked fixing before the step re-executes. A step whose fix
// round ends with ordinary execution (the CI monitor after a published repair)
// reports that through MarkRunning, and the durable status plus the event
// stream both read running again before Execute returns.
func TestExecutor_MarkRunningReturnsAFixingStepToRunning(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var statuses []string
	onEvent := func(event ipc.Event) {
		if event.StepName == nil || *event.StepName != types.StepCI || event.Status == nil {
			return
		}
		statuses = append(statuses, string(event.Type)+":"+*event.Status)
	}

	calls := 0
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"error","description":"CI check failing: test","action":"auto-fix","category":"ci-check","check":"test"}],"summary":"1 CI check failing"}`,
			}, nil
		}
		before, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil || before == nil || before.Status != types.StepStatusFixing {
			t.Errorf("status before MarkRunning = %v (%v), want fixing", before, err)
		}
		if sctx.MarkRunning == nil {
			t.Fatal("MarkRunning must be wired on every step context")
		}
		if err := sctx.MarkRunning(); err != nil {
			t.Fatalf("MarkRunning() error = %v", err)
		}
		after, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil || after == nil || after.Status != types.StepStatusRunning {
			t.Errorf("status after MarkRunning = %v (%v), want running", after, err)
		}
		return &StepOutcome{}, nil
	}}

	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, []Step{step}, onEvent)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("step calls = %d, want the observation and one fix round", calls)
	}
	sawFixing, sawRunningAfterFixing := false, false
	for _, status := range statuses {
		switch {
		case status == string(ipc.EventStepCompleted)+":"+string(types.StepStatusFixing):
			sawFixing = true
		case sawFixing && status == string(ipc.EventStepStarted)+":"+string(types.StepStatusRunning):
			sawRunningAfterFixing = true
		}
	}
	if !sawFixing || !sawRunningAfterFixing {
		t.Fatalf("events = %v, want fixing followed by a running step_started", statuses)
	}
}
