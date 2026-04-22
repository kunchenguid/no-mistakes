package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_FixEmitsDiffAndFixReviewStatus(t *testing.T) {
	database, p, run, repo := setupTest(t)

	// Create a real git repo as workDir so DiffHead works
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Step that needs approval on first call and after fix
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if sctx.Fixing {
				// Simulate agent making changes in the worktree
				writeTestFile(t, workDir, "fix.txt", "agent fix\n")
				execGit(t, workDir, "add", "fix.txt")
			}
			return &StepOutcome{NeedsApproval: true, Findings: `{"items":[]}`}, nil
		},
	}

	steps := []Step{step}
	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// First: step reaches awaiting_approval (not fix_review)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Verify initial event has awaiting_approval status
	initialEvent := waitForStepEvent(t, events, ipc.EventStepCompleted, types.StepReview)
	if initialEvent.Status == nil || *initialEvent.Status != string(types.StepStatusAwaitingApproval) {
		t.Errorf("expected awaiting_approval status, got %v", initialEvent.Status)
	}
	if initialEvent.Diff != nil {
		t.Error("expected no diff on initial approval")
	}

	// Send fix action
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// Find the fix_review event
	fixEvent := waitForEvent(t, events, ipc.EventStepCompleted, string(types.StepStatusFixReview))

	// Verify diff is included in the event
	if fixEvent.Diff == nil || *fixEvent.Diff == "" {
		t.Error("expected diff in fix_review event")
	} else if !strings.Contains(*fixEvent.Diff, "fix.txt") {
		t.Errorf("expected diff to mention fix.txt, got: %s", *fixEvent.Diff)
	}

	// Approve to end
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

func TestExecutor_FixEmitsFixingStatusImmediately(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	fixStarted := make(chan struct{})
	releaseFix := make(chan struct{})
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"issues":["bug"]}`}, nil
			}
			close(fixStarted)
			<-releaseFix
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixing)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if event := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing)); event != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if event := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing)); event == nil {
		close(releaseFix)
		<-done
		t.Fatal("expected step_completed event with fixing status after fix was accepted")
	}

	<-fixStarted
	close(releaseFix)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_FixReviewNoChanges(t *testing.T) {
	database, p, run, repo := setupTest(t)

	// Create a real git repo as workDir
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Step that needs approval both times but agent makes no changes on fix
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{NeedsApproval: true, Findings: `{"items":[]}`}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// No changes made — diff should not be in event
	fixEvent := waitForEvent(t, events, ipc.EventStepCompleted, string(types.StepStatusFixReview))
	if fixEvent.Diff != nil {
		t.Error("expected no diff when agent made no changes")
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

func TestExecutor_FixSetsPreviousFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	findings := `{"findings":[{"severity":"error","file":"main.go","line":42,"description":"nil pointer dereference","action":"auto-fix"}],"summary":"1 error found"}`
	var capturedFindings string

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// First call: return findings that need approval
				return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
			}
			// Second call (fix): capture PreviousFindings and pass
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionFix, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	items := mustParseFindingItems(t, capturedFindings)
	if len(items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(items))
	}
	if items[0].ID != "review-1" || items[0].Description != "nil pointer dereference" {
		t.Errorf("unexpected PreviousFindings: %#v", items)
	}
}

func TestExecutor_AssignsFindingIDsBeforePersistingAndEmitting(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"first","action":"auto-fix"},{"severity":"warning","description":"second","action":"auto-fix"}],"summary":"2 findings"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	paused := waitForStepEvent(t, events, ipc.EventStepCompleted, types.StepReview)
	if paused.Findings == nil {
		t.Fatal("expected paused step event with findings")
	}

	items := mustParseFindingItems(t, *paused.Findings)
	if len(items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(items))
	}
	if items[0].ID != "review-1" || items[1].ID != "review-2" {
		t.Fatalf("unexpected finding IDs: %#v", items)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("expected findings stored in DB")
	}
	storedItems := mustParseFindingItems(t, *steps[0].FindingsJSON)
	if len(storedItems) != 2 {
		t.Fatalf("expected 2 stored findings, got %d", len(storedItems))
	}
	if storedItems[0].ID != "review-1" || storedItems[1].ID != "review-2" {
		t.Fatalf("unexpected stored finding IDs: %#v", storedItems)
	}

	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestExecutor_FixAppliesUserInstructionsAndAddedFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"second","action":"auto-fix"}],"summary":"2 findings"}`,
				}, nil
			}
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	instructions := map[string]string{"review-1": "only touch parser.go, skip helpers"}
	added := []types.Finding{{Severity: "warning", Description: "also audit logger init", Action: types.ActionAutoFix}}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, instructions, added); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	items := mustParseFindingItems(t, capturedFindings)
	if len(items) != 2 {
		t.Fatalf("expected 2 findings (selected + user-added), got %d: %s", len(items), capturedFindings)
	}
	if items[0].ID != "review-1" {
		t.Errorf("expected selected agent finding first, got %q", items[0].ID)
	}
	if items[0].UserInstructions != "only touch parser.go, skip helpers" {
		t.Errorf("expected instruction attached to review-1, got %q", items[0].UserInstructions)
	}
	if items[1].ID != "user-1" {
		t.Errorf("expected user-added finding to get ID user-1, got %q", items[1].ID)
	}
	if items[1].Source != types.FindingSourceUser {
		t.Errorf("expected user-added finding to be tagged source=user, got %q", items[1].Source)
	}

	rounds, err := database.GetRoundsByStep(firstStepID(t, database, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) == 0 {
		t.Fatal("expected at least one round")
	}
	round := rounds[0]
	if round.UserFindingsJSON == nil {
		t.Fatal("expected user_findings_json to be persisted on the selection round")
	}
	if !strings.Contains(*round.UserFindingsJSON, "audit logger init") {
		t.Errorf("expected user findings payload to include user-added description, got %s", *round.UserFindingsJSON)
	}
	if round.SelectedFindingIDs == nil {
		t.Fatal("expected selected_finding_ids to be set")
	}
	if !strings.Contains(*round.SelectedFindingIDs, "user-1") {
		t.Errorf("expected user-added finding id in selected list, got %s", *round.SelectedFindingIDs)
	}
}

func firstStepID(t *testing.T, database *db.DB, runID string) string {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("no steps persisted")
	}
	return steps[0].ID
}

func TestExecutor_FixUsesSelectedFindingIDsOnly(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"second","action":"auto-fix"}],"summary":"2 findings"}`,
				}, nil
			}
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	items := mustParseFindingItems(t, capturedFindings)
	if len(items) != 1 {
		t.Fatalf("expected 1 selected finding, got %d", len(items))
	}
	if items[0].ID != "review-2" || items[0].Description != "second" {
		t.Fatalf("unexpected selected finding: %#v", items[0])
	}
}

func TestExecutor_FixClearsStoredFindingsAfterSuccessfulReRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"severity":"error","description":"first pass issue","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON != nil {
		t.Fatalf("expected findings to be cleared, got %q", *dbSteps[0].FindingsJSON)
	}
}

func TestExecutor_FixPersistsFollowUpRoundAsAutoFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"severity":"error","description":"first pass issue","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}

	rounds, err := database.GetRoundsByStep(dbSteps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(rounds))
	}
	if rounds[0].Trigger != "initial" {
		t.Fatalf("round 1 trigger = %q, want %q", rounds[0].Trigger, "initial")
	}
	if rounds[1].Trigger != "auto_fix" {
		t.Fatalf("round 2 trigger = %q, want %q", rounds[1].Trigger, "auto_fix")
	}
}

func TestExecutor_FixSelectedFindingsRewritesSummary(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"second","action":"auto-fix"}],"summary":"2 findings"}`,
				}, nil
			}
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	var payload struct {
		Findings []findingJSON `json:"findings"`
		Summary  string        `json:"summary"`
	}
	if err := json.Unmarshal([]byte(capturedFindings), &payload); err != nil {
		t.Fatalf("parse findings JSON: %v", err)
	}
	if len(payload.Findings) != 1 || payload.Findings[0].ID != "review-2" {
		t.Fatalf("unexpected selected findings payload: %#v", payload.Findings)
	}
	if payload.Summary != "1 selected finding" {
		t.Fatalf("summary = %q, want %q", payload.Summary, "1 selected finding")
	}
}

func TestExecutor_UserFixRecordsSelectedFindingIDsAndFixSummary(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"second","action":"auto-fix"}],"summary":"2 findings"}`,
				}, nil
			}
			return &StepOutcome{FixSummary: "fix the warning"}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	rounds, err := database.GetRoundsByStep(dbSteps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(rounds))
	}

	if rounds[0].SelectedFindingIDs == nil {
		t.Fatal("expected selected_finding_ids set on round 1")
	}
	var ids []string
	if err := json.Unmarshal([]byte(*rounds[0].SelectedFindingIDs), &ids); err != nil {
		t.Fatalf("parse selected_finding_ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "review-2" {
		t.Fatalf("unexpected selected ids: %v", ids)
	}

	if rounds[1].FixSummary == nil || *rounds[1].FixSummary != "fix the warning" {
		t.Fatalf("expected fix_summary %q on round 2, got %v", "fix the warning", rounds[1].FixSummary)
	}
}

func TestExecutor_AutoFixRecordsSelectedFindingIDs(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					AutoFixable: true,
					Findings:    `{"findings":[{"id":"review-1","severity":"warning","description":"a","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"b","action":"ask-user"}],"summary":"2"}`,
				}, nil
			}
			return &StepOutcome{FixSummary: "apply cheap fix"}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(dbSteps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("expected 2 rounds, got %d", len(rounds))
	}
	if rounds[0].SelectedFindingIDs == nil {
		t.Fatal("expected selected_finding_ids set on round 1 after auto-fix")
	}
	var ids []string
	if err := json.Unmarshal([]byte(*rounds[0].SelectedFindingIDs), &ids); err != nil {
		t.Fatalf("parse selected_finding_ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "review-1" {
		t.Fatalf("expected only auto-fixable id to be recorded, got %v", ids)
	}
	if rounds[1].FixSummary == nil || *rounds[1].FixSummary != "apply cheap fix" {
		t.Fatalf("expected fix_summary persisted on round 2, got %v", rounds[1].FixSummary)
	}
}

func TestRoundInsertIDClearsOnInsertFailure(t *testing.T) {
	round := &db.StepRound{ID: "round-2"}
	if got := roundInsertID("round-1", round, nil); got != "round-2" {
		t.Fatalf("roundInsertID success = %q, want %q", got, "round-2")
	}
	if got := roundInsertID("round-1", nil, context.Canceled); got != "" {
		t.Fatalf("roundInsertID failure = %q, want empty", got)
	}
}

func TestExecutor_StepResultIDIsExposedToSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedStepResultID string
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			capturedStepResultID = sctx.StepResultID
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if capturedStepResultID == "" {
		t.Fatal("expected StepContext.StepResultID to be populated")
	}
	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbSteps) != 1 || dbSteps[0].ID != capturedStepResultID {
		t.Fatalf("StepResultID did not match the step's DB row (got %q, want %q)", capturedStepResultID, dbSteps[0].ID)
	}
}

func TestExecutor_PreviousFindingsEmptyOnFirstExecution(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if capturedFindings != "" {
		t.Errorf("PreviousFindings should be empty on first execution, got: %s", capturedFindings)
	}
}
