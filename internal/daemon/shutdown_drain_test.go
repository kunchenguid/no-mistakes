package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type drainStep struct {
	started chan struct{}
	release chan struct{}
	outcome *pipeline.StepOutcome
}

func (*drainStep) Name() types.StepName { return types.StepDocument }
func (s *drainStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	close(s.started)
	select {
	case <-s.release:
		if s.outcome != nil {
			return s.outcome, nil
		}
		return &pipeline.StepOutcome{ExitCode: 0}, nil
	case <-ctx.Ctx.Done():
		return nil, context.Cause(ctx.Ctx)
	}
}

func TestShutdown_DrainsCurrentStepAndRefusesNewRuns(t *testing.T) {
	p, database := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, database, "shutdown-drain")
	step := &drainStep{started: make(chan struct{}), release: make(chan struct{})}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	runID, err := manager.startRun(context.Background(), repo, "main", head, head, "test", nil, "drain", "", false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-step.started:
	case <-time.After(10 * time.Second):
		t.Fatal("step did not start")
	}
	stopped := make(chan struct{})
	go func() { manager.Shutdown(); close(stopped) }()
	deadline := time.Now().Add(5 * time.Second)
	for !manager.shuttingDown.Load() {
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := manager.startRun(context.Background(), repo, "other", head, head, "test", nil, "drain", "", false, "", nil); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("new run during drain error = %v", err)
	}
	close(step.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish after the step completed")
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted || run.Error != nil {
		t.Fatalf("drained run = %s error %v, want completed without error", run.Status, run.Error)
	}
	results, err := database.GetStepsByRun(runID)
	if err != nil || len(results) != 1 || results[0].Status != types.StepStatusCompleted {
		t.Fatalf("drained steps = %+v, %v", results, err)
	}
}
