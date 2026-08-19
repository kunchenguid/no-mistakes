package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testCertifiedHead = "1111111111111111111111111111111111111111"

func TestExecutorCapturesReviewFleetModeBeforeExecution(t *testing.T) {
	database, p, run, repo := setupTest(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, testReviewFleetConfig(bin), nil, nil, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("expected fleet run without mandatory gates to fail")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReviewFleetEnabled || !run.ReviewFleetEnabled || got.ReviewFleetFingerprint == nil || run.ReviewFleetFingerprint == nil {
		t.Fatalf("fleet contract was not captured: durable=%#v in-memory=%#v", got, run)
	}
}

func TestRecoveredFleetRequiresExactOriginalContract(t *testing.T) {
	database, p, run, _ := setupTest(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original := testReviewFleetConfig(bin)
	originalSettings, err := reviewFleetSettingsFromConfig(original)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := reviewFleetFingerprint(originalSettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewFleetMode(run.ID, true, &fingerprint); err != nil {
		t.Fatal(err)
	}
	run.ReviewFleetEnabled = true
	run.ReviewFleetFingerprint = &fingerprint

	same := NewExecutor(database, p, testReviewFleetConfig(bin), nil, nil, nil)
	same.initializeRunScopes(run.ID)
	if err := same.validateRecoveredReviewFleet(run); err != nil {
		t.Fatalf("unchanged recovered contract rejected: %v", err)
	}

	changedConfig := testReviewFleetConfig(bin)
	changedConfig.ReviewFleet.Certifier.ReasoningEffort = "high"
	changed := NewExecutor(database, p, changedConfig, nil, nil, nil)
	changed.initializeRunScopes(run.ID)
	if err := changed.validateRecoveredReviewFleet(run); err == nil || !strings.Contains(err.Error(), "contract changed") {
		t.Fatalf("changed recovered contract was accepted: %v", err)
	}
}

func TestExecutor_CertifyAuthorityOnlyCompletesOrIsExplicitlyApproved(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		workDir, candidate := setupCertificationApprovalWorktree(t)
		bindCertificationRunHead(t, database, run, candidate)
		step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{CertifiedHeadSHA: candidate}}
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
			t.Fatal(err)
		}
		assertCertifiedHead(t, database, run.ID, candidate)
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
	workDir, candidate := setupCertificationApprovalWorktree(t)
	bindCertificationRunHead(t, database, run, candidate)
	step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{
		NeedsApproval:    true,
		CertifiedHeadSHA: candidate,
		Findings:         `{"findings":[{"id":"cert-1","severity":"error","description":"operator decision","action":"ask-user"}]}`,
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCertify, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCertify, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := waitExecutor(t, done); err != nil {
		t.Fatal(err)
	}
	assertCertifiedHead(t, database, run.ID, candidate)
}

func TestExecutor_ApprovedCertifyGateRejectsChangedCandidate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir, candidate := setupCertificationApprovalWorktree(t)
	bindCertificationRunHead(t, database, run, candidate)
	step := &mockStep{name: types.StepCertify, outcome: &StepOutcome{
		NeedsApproval:    true,
		CertifiedHeadSHA: candidate,
		Findings:         `{"findings":[{"id":"cert-1","severity":"error","description":"operator decision","action":"ask-user"}]}`,
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCertify, types.StepStatusAwaitingApproval)
	if err := os.WriteFile(workDir+"/changed.txt", []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Respond(types.StepCertify, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := waitExecutor(t, done); err == nil || !strings.Contains(err.Error(), "certify worktree is dirty before approval") {
		t.Fatalf("changed candidate was accepted: %v", err)
	}
	assertNoCertifiedHead(t, database, run.ID)
}

func setupCertificationApprovalWorktree(t *testing.T) (string, string) {
	t.Helper()
	workDir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		if _, err := git.Run(ctx, workDir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(workDir+"/tracked.txt", []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, workDir, "add", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, workDir, "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		t.Fatal(err)
	}
	return workDir, head
}

func bindCertificationRunHead(t *testing.T, database *db.DB, run *db.Run, head string) {
	t.Helper()
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
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
