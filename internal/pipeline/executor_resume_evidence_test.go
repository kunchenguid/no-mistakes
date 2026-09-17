package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// evidenceReadingResumer is an ApprovalGateResumer with ReviewStep's shape: it
// resolves the run's evidence directory from the StepContext and can only
// resume once it has one, exactly as ReviewStep.ResumeApprovalGate resolves the
// conversation directory from reviewqa.Dir(sctx.EvidenceDir).
type evidenceReadingResumer struct {
	name      types.StepName
	fn        func(sctx *StepContext) (*StepOutcome, error)
	mu        sync.Mutex
	seen      []string
	askedOnce bool
}

func (s *evidenceReadingResumer) Name() types.StepName { return s.name }

func (s *evidenceReadingResumer) Execute(sctx *StepContext) (*StepOutcome, error) {
	return s.fn(sctx)
}

func (s *evidenceReadingResumer) ResumeApprovalGate(sctx *StepContext, _ string) (types.ApprovalAction, bool, error) {
	s.mu.Lock()
	s.seen = append(s.seen, sctx.EvidenceDir)
	s.askedOnce = true
	s.mu.Unlock()
	if sctx.EvidenceDir == "" {
		// Declining here is what the real step does, and it is why an omitted
		// EvidenceDir is silent rather than an error.
		return "", false, nil
	}
	return types.ActionAnswer, true, nil
}

func (s *evidenceReadingResumer) evidenceDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// TestExecutor_RecoveredGateResumerReceivesTheRunsEvidenceDir drives the
// DAEMON RESTART path, which is the only path that got this wrong and the only
// path a gate resumer exists for: a park lasts tens of minutes to hours, so a
// restart inside that window is precisely the window ReviewStep's resumer
// closes.
//
// Resume builds its own StepContext for the recovered gate rather than reusing
// executeStep's, and that literal omitted EvidenceDir. A resumer that reads the
// run's evidence then resolves an empty path and declines on every tick,
// silently - it returns (false, nil) for a missing directory exactly as it does
// for a conversation with nothing to resume - so the recovered gate parks
// forever with no error anywhere. The live loop sets EvidenceDir, so a test
// driving Execute passes on the broken code and proves nothing.
func TestExecutor_RecoveredGateResumerReceivesTheRunsEvidenceDir(t *testing.T) {
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
	findings := `{"findings":[{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: keep the legacy route?","action":"ask-user","category":"review-question"}],"summary":"one open question"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "", 25); err != nil {
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

	resumed := make(chan struct{})
	step := &evidenceReadingResumer{name: types.StepReview}
	step.fn = func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.FinalizingAnswers {
			return nil, fmt.Errorf("the resumed gate did not re-enter the step as a finalize turn")
		}
		close(resumed)
		return &StepOutcome{}, nil
	}

	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 2}}, newFakeSessionAgent(), []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, time.Second)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatalf("the recovered gate never resumed; the resumer saw evidence dirs %q", step.evidenceDirs())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	// The exact value executeStep supplies, so the two paths cannot drift.
	want := exec.runEvidenceDir(run.ID)
	if want == "" {
		t.Fatal("the test executor resolved no evidence dir, so it cannot prove the recovered one")
	}
	seen := step.evidenceDirs()
	if !step.askedOnce {
		t.Fatal("the recovered gate never consulted its resumer")
	}
	for _, got := range seen {
		if got != want {
			t.Fatalf("resumer was given evidence dir %q, want %q", got, want)
		}
	}
}
