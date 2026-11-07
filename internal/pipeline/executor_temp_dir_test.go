package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// tempDirStep records the StepContext.TempDir the executor handed it.
type tempDirStep struct {
	name types.StepName
	got  string
}

func (s *tempDirStep) Name() types.StepName { return s.name }

func (s *tempDirStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.got = sctx.TempDir
	return &StepOutcome{ExitCode: 0}, nil
}

// TestExecutor_StepsReceiveTheRunTempDir pins the wiring behind issue #1519:
// every step's agent turns get the run's own temp directory as TMPDIR (see
// withRunTempDir), which is the directory run cleanup sweeps and removes, so
// the executor must hand each step exactly paths.RunTempDir for its run.
func TestExecutor_StepsReceiveTheRunTempDir(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &tempDirStep{name: types.StepReview}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := p.RunTempDir(repo.ID, run.ID); step.got != want {
		t.Fatalf("StepContext.TempDir = %q, want %q", step.got, want)
	}
}
