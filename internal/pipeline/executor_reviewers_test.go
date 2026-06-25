package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stubReviewer is a minimal agent.Agent used only to assert identity and order
// of the reviewers a step observes via StepContext.Reviewers. It never runs.
type stubReviewer struct{ name string }

func (s *stubReviewer) Name() string { return s.name }
func (s *stubReviewer) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}
func (s *stubReviewer) Close() error { return nil }

// captureReviewers builds a one-step pipeline whose review step records the
// reviewers it sees, runs it to completion, and returns the captured slice.
// Running Execute in a goroutine mirrors the existing executor_context_test.go
// style; the done channel establishes a happens-before with the capture so the
// read below is race-free under -race.
func captureReviewers(t *testing.T, ag agent.Agent, configure func(*Executor)) []agent.Agent {
	t.Helper()
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	captured := make(chan []agent.Agent, 1)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			cp := make([]agent.Agent, len(sctx.Reviewers))
			copy(cp, sctx.Reviewers)
			captured <- cp
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	if configure != nil {
		configure(exec)
	}

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	select {
	case got := <-captured:
		return got
	default:
		t.Fatal("review step did not record reviewers")
		return nil
	}
}

// TestExecutor_ReviewersDefaultAndPassthrough pins the PR1 invariant: with no
// SetReviewers the review panel defaults to exactly the single impl agent, and
// after SetReviewers([a, b]) the two reviewers pass through to StepContext in
// order. Both halves run deterministically with no real agent calls.
func TestExecutor_ReviewersDefaultAndPassthrough(t *testing.T) {
	t.Run("DefaultsToSingleImplAgent", func(t *testing.T) {
		impl := &stubReviewer{name: "impl"}

		got := captureReviewers(t, impl, nil) // no SetReviewers

		if len(got) != 1 {
			t.Fatalf("default reviewers len = %d, want 1 (the single impl agent)", len(got))
		}
		if got[0] != agent.Agent(impl) {
			t.Errorf("default reviewer = %v (Name=%q), want the impl agent itself", got[0], got[0].Name())
		}
	})

	t.Run("PassesConfiguredPanelInOrder", func(t *testing.T) {
		impl := &stubReviewer{name: "impl"}
		a := &stubReviewer{name: "a"}
		b := &stubReviewer{name: "b"}

		got := captureReviewers(t, impl, func(exec *Executor) {
			exec.SetReviewers([]agent.Agent{a, b})
		})

		if len(got) != 2 {
			t.Fatalf("panel reviewers len = %d, want 2", len(got))
		}
		if got[0] != agent.Agent(a) || got[1] != agent.Agent(b) {
			t.Fatalf("panel reviewers = [%q, %q], want [a, b] in order", got[0].Name(), got[1].Name())
		}
	})
}
