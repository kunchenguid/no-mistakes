package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testCertifiedHead = "1111111111111111111111111111111111111111"

func TestExecutor_CertifyAuthorityOnlyCompletesOrIsExplicitlyApproved(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{CertifiedHeadSHA: testCertifiedHead}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		assertCertifiedHead(t, database, run.ID, testCertifiedHead)
	})

	t.Run("parked then skipped", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{
			NeedsApproval:    true,
			CertifiedHeadSHA: testCertifiedHead,
			Findings:         `{"findings":[{"id":"cert-1","severity":"warning","description":"inspect","action":"ask-user"}]}`,
		}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		done := make(chan error, 1)
		go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
		waitForStepStatus(t, database, run.ID, types.StepCertify, types.StepStatusAwaitingApproval)
		assertNoCertifiedHead(t, database, run.ID)
		if err := exec.Respond(types.StepCertify, types.ActionSkip, nil); err != nil {
			t.Fatal(err)
		}
		if err := waitExecutor(t, done); err != nil {
			t.Fatal(err)
		}
		assertNoCertifiedHead(t, database, run.ID)
	})

	t.Run("failed", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		step := newFailStep(types.StepCertify, errors.New("certifier unavailable"))
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
			t.Fatal("expected failed certification")
		}
		assertNoCertifiedHead(t, database, run.ID)
	})

	t.Run("fix request fails without certification", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		step := &adaptiveCallStep{name: types.StepCertify, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.Fixing {
				return nil, errors.New("certify step does not support fixes")
			}
			return &StepOutcome{
				NeedsApproval:    true,
				CertifiedHeadSHA: testCertifiedHead,
				Findings:         `{"findings":[{"id":"cert-1","severity":"error","description":"must repair","action":"ask-user"}]}`,
			}, nil
		}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		done := make(chan error, 1)
		go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
		waitForStepStatus(t, database, run.ID, types.StepCertify, types.StepStatusAwaitingApproval)
		if err := exec.Respond(types.StepCertify, types.ActionFix, nil); err != nil {
			t.Fatal(err)
		}
		if err := waitExecutor(t, done); err == nil {
			t.Fatal("Fix unexpectedly completed Certify")
		}
		assertNoCertifiedHead(t, database, run.ID)
	})

	t.Run("cancelled", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{CertifiedHeadSHA: testCertifiedHead}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		if err := exec.Execute(ctx, run, repo, t.TempDir()); err == nil {
			t.Fatal("expected cancelled certification run")
		}
		assertNoCertifiedHead(t, database, run.ID)
	})
}

func TestExecutor_ApprovedCertifyGateBindsExactCandidate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const candidate = "2222222222222222222222222222222222222222"
	step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{
		NeedsApproval:    true,
		CertifiedHeadSHA: candidate,
		Findings:         `{"findings":[{"id":"cert-1","severity":"error","description":"operator decision","action":"ask-user"}]}`,
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCertify, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCertify, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := waitExecutor(t, done); err != nil {
		t.Fatal(err)
	}
	assertCertifiedHead(t, database, run.ID, candidate)
}

func assertCertifiedHead(t *testing.T, database interface {
	GetRun(string) (*db.Run, error)
}, runID, want string) {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CertifiedHeadSHA == nil || *run.CertifiedHeadSHA != want {
		t.Fatalf("certified head = %#v, want %s", run.CertifiedHeadSHA, want)
	}
}

func assertNoCertifiedHead(t *testing.T, database interface {
	GetRun(string) (*db.Run, error)
}, runID string) {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CertifiedHeadSHA != nil {
		t.Fatalf("unexpected certified head authority: %#v", run.CertifiedHeadSHA)
	}
}

func waitExecutor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish")
		return nil
	}
}
