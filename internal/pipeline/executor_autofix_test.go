package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AutoFixTriggersWithoutApproval(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix enabled for review (max 3 attempts)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			// After auto-fix, verify Fixing is set
			if !sctx.Fixing {
				t.Error("expected Fixing to be true on auto-fix re-execution")
			}
			if sctx.PreviousFindings == "" {
				t.Error("expected PreviousFindings to be set on auto-fix")
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected step called 2 times (initial + auto-fix), got %d", callCount)
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}
}

func TestExecutor_AutoFixRespectsMaxAttempts(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix limited to 2 attempts for lint
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 2}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			// Always return NeedsApproval to exhaust auto-fix attempts
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"warning","description":"style issue","action":"auto-fix"}],"summary":"lint issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// After 2 auto-fix attempts fail, should fall back to manual approval
	// 1 initial + 2 auto-fix = 3 calls, then waits for approval
	// Status is fix_review since auto-fix cycles ran (sctx.Fixing was true)
	waitForStepStatus(t, database, run.ID, types.StepLint, types.StepStatusFixReview)

	if callCount != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 auto-fix), got %d", callCount)
	}

	// Now approve manually to finish
	exec.Respond(types.StepLint, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixDisabledWithZero(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix disabled for review
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Should immediately wait for approval (no auto-fix)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixNilConfigUsesDefaults(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// nil config - executor should not panic and should use no auto-fix (backwards compat)
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// With nil config, should wait for approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (nil config, no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionAbort, nil)
	<-done
}

func TestExecutor_AutoFixEmitsEvents(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 1}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"warning","description":"issue","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have a fixing status event from auto-fix
	fixingEvent := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing))
	if fixingEvent == nil {
		t.Error("expected step_completed event with fixing status during auto-fix")
	}
}

func TestExecutor_DoesNotAutoFixManualApprovalOutcome(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Test: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"info","description":"new test file written by agent: agent_test.go","action":"no-op"}],"summary":"tests passed, but agent wrote new test files"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Fatalf("expected 1 call for manual approval outcome, got %d", callCount)
	}

	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixInfoFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// Info findings that are auto-fixable (not blocking, but fixable)
				return &StepOutcome{
					NeedsApproval: false,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"info","description":"could simplify","action":"auto-fix"}],"summary":"1 suggestion"}`,
				}, nil
			}
			// After auto-fix, step passes clean
			if !sctx.Fixing {
				t.Error("expected Fixing to be true on auto-fix re-execution")
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 calls (initial + auto-fix), got %d", callCount)
	}
}

func TestExecutor_AutoFixSkipsHumanReviewFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			// All findings are ask-user - auto-fix should not trigger
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Should go straight to user approval without auto-fix
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Fatalf("expected 1 call (no auto-fix for ask-user findings), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_HumanReviewFindingsRequireApprovalWithoutNeedsApprovalFlag(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: false,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"info","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 3}}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixMixedFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// Mix: one auto-fixable, one ask-user
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings: `{"findings":[
						{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"},
						{"id":"review-2","severity":"warning","description":"design choice","action":"ask-user"}
					],"summary":"2 issues","risk_level":"medium","risk_rationale":"mixed"}`,
				}, nil
			}
			// After auto-fix: verify only fixable finding was sent
			if sctx.PreviousFindings == "" {
				t.Error("expected PreviousFindings")
			}
			parsed, _ := types.ParseFindingsJSON(sctx.PreviousFindings)
			if len(parsed.Items) != 1 {
				t.Errorf("expected 1 fixable finding passed to fix, got %d", len(parsed.Items))
			}
			if len(parsed.Items) > 0 && parsed.Items[0].Description != "bug" {
				t.Errorf("expected fixable finding 'bug', got %q", parsed.Items[0].Description)
			}
			// Return only the ask-user finding remaining
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"review-2","severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// After auto-fixing the bug, only ask-user finding remains.
	// No more fixable findings, so falls through to user approval.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	if callCount != 2 {
		t.Errorf("expected 2 calls (initial + 1 auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}
