package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The review step's gate decides on an append-only outstanding set rather than
// on one round's output: a selected finding stays outstanding until a later
// round positively records that it re-checked the finding's file and no longer
// reports the defect, or until the operator approves, skips, or aborts the
// gate. See resolveVerifiedFindingsJSON and Executor.executeStep.

const reviewCarryTwoFindings = `{"findings":[` +
	`{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref on the error path","action":"ask-user"},` +
	`{"id":"review-2","severity":"warning","file":"cache.go","line":42,"description":"unbounded cache growth","action":"ask-user"}],` +
	`"summary":"2 findings"}`

func seedRecoveredReviewGate(t *testing.T, database *db.DB, run *db.Run, findings string, status types.StepStatus, selectedIDs string) (*db.StepResult, *db.Run) {
	t.Helper()
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
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "reviewed-head", 25)
	if err != nil {
		t.Fatal(err)
	}
	if selectedIDs != "" {
		if err := database.SetStepRoundUserDecision(round.ID, &selectedIDs, db.RoundSelectionSourceUser, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, status, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	recoveredRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stepResult, recoveredRun
}

func TestExecutor_ReviewCarryForward_RecoverySeedsPendingVerification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"}],"summary":"1 finding"}`
	stepResult, recoveredRun := seedRecoveredReviewGate(t, database, run, findings, types.StepStatusFixReview, `["review-1"]`)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{ReviewedPaths: []string{"service.go"}, ReviewablePaths: []string{"service.go"}}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if respondErr = exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); respondErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond to recovered review: %v", respondErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("recovered review did not complete after positive verification")
	}

	got, err := database.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FindingsJSON != nil {
		t.Fatalf("verified recovered finding remained outstanding: %s", *got.FindingsJSON)
	}
}

func TestExecutor_ReviewCarryForward_RecoveryPersistsRemappedSelection(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[` +
		`{"id":"user-1","severity":"warning","file":"old.go","description":"old carried issue","action":"ask-user"},` +
		`{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"}],"summary":"2 findings"}`
	_, recoveredRun := seedRecoveredReviewGate(t, database, run, findings, types.StepStatusAwaitingApproval, "")
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, ReviewedPaths: []string{"service.go", "new.go"}, ReviewablePaths: []string{"service.go", "new.go"}}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

	added := []types.Finding{{ID: "user-1", Severity: types.FindingSeverityInfo, File: "new.go", Description: "new user note", Action: types.ActionNoOp}}
	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if respondErr = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, added, ""); respondErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond to recovered review: %v", respondErr)
	}
	deadline = time.Now().Add(5 * time.Second)
	var rounds []*db.StepRound
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) > 0 {
			rounds, err = database.GetRoundsByStep(steps[0].ID)
			if err == nil && len(rounds) >= 2 && steps[0].Status == types.StepStatusFixReview {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(rounds) < 2 {
		t.Fatal("recovered review did not reach its rereview gate")
	}
	selected := findingIDsFromSelectionJSON(derefString(rounds[0].SelectedFindingIDs))
	if !containsString(selected, "review-2") || containsString(selected, "user-1") {
		t.Fatalf("recovered selection IDs = %v, want remapped review-2 without stale user-1", selected)
	}
	if rounds[0].UserFindingsJSON == nil {
		t.Fatal("expected remapped user findings to be persisted")
	}
	persistedUserFindings, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
	if err != nil {
		t.Fatalf("parse persisted user findings: %v", err)
	}
	if !containsFindingID(persistedUserFindings.Items, "review-2") || containsFindingID(persistedUserFindings.Items, "user-1") {
		t.Fatalf("persisted user finding IDs = %v, want remapped review-2 without user-1", findingIDs(persistedUserFindings.Items))
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered review did not finish after approval")
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFindingID(items []types.Finding, want string) bool {
	for _, item := range items {
		if item.ID == want {
			return true
		}
	}
	return false
}

func findingIDs(items []types.Finding) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestExecutor_ReviewCarryForward_NoOpFixKeepsFindingParked mirrors the journey
// that made the predecessor carry design (PR #704) a work-loss hole, inverted to
// prove the hole is closed: the review reports two ask-user findings, the
// operator selects one, the fixer writes a commit that does not fix it, and the
// rereview reports nothing new without positively covering the finding's file.
// The selected finding must still be outstanding, blocking the gate, with its
// identity and action intact - the run must park again, never pass.
func TestExecutor_ReviewCarryForward_NoOpFixKeepsFindingParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        reviewCarryTwoFindings,
					ReviewedPaths:   []string{"service.go", "cache.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			}
			// The fixer writes a commit that does not fix the selected defect.
			if err := os.WriteFile(filepath.Join(workDir, "unrelated.txt"), []byte("tidy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			execGit(t, workDir, "add", "unrelated.txt")
			execGit(t, workDir, "commit", "-m", "tidy unrelated code")
			// The rereview reports nothing new and offers no coverage record for
			// service.go: it did not look there, so nothing about the finding is
			// proven. Silence may never read as resolution.
			return &StepOutcome{FixSummary: "tidy unrelated code"}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}

	// The gate re-parks instead of the run completing: the rereview that
	// reported nothing new did not verify the selected finding.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("selected finding was dropped from the outstanding set before anything verified it")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	var selected *types.Finding
	for i := range parsed.Items {
		if parsed.Items[i].ID == "review-1" {
			selected = &parsed.Items[i]
		}
	}
	if selected == nil {
		t.Fatalf("selected finding review-1 is not outstanding after a no-op fix: %s", *steps[0].FindingsJSON)
	}
	if selected.Action != types.ActionAskUser {
		t.Errorf("selected finding action = %q, want %q", selected.Action, types.ActionAskUser)
	}
	if selected.Description != "nil deref on the error path" {
		t.Errorf("selected finding description = %q, want the original", selected.Description)
	}

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status == types.RunCompleted {
		t.Fatal("run reached a completed outcome with the selected finding unresolved")
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("expected the run to be parked awaiting the operator")
	}

	// The stats have to derive from the same outstanding set the gate uses:
	// nothing is fixed while the operator is still parked on it.
	stats, err := database.StepFindingStats(steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if stats.FixedFindings != 0 {
		t.Errorf("stats reported %d fixed findings while the operator is still parked", stats.FixedFindings)
	}

	// Approving is the explicit operator action that clears what silence may
	// not: the operator may still ship past the finding deliberately.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_PositiveCoverageClearsFinding is the other
// half of the same contract: a selected finding does leave the outstanding set
// once the rereview positively records that it covered the finding's file and
// no longer reports the defect. Without this the carry set could only grow and
// a verified fix would park forever.
func TestExecutor_ReviewCarryForward_UserAddedFindingStaysOutstanding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"nil deref","action":"ask-user"}],"summary":"1 finding"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	added := []types.Finding{{Severity: types.FindingSeverityWarning, File: "logger.go", Description: "audit logger setup", Action: types.ActionAskUser}}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, added, ""); err != nil {
		t.Fatalf("fix with added finding: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	found := false
	for _, item := range parsed.Items {
		if item.ID == "user-1" && item.Description == "audit logger setup" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user-added finding was dropped from outstanding carry: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_ReviewCarryForward_RemintsUserAddedCollisionForPendingVerification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings: `{"findings":[` +
						`{"id":"user-1","severity":"warning","file":"old.go","description":"old carried issue","action":"ask-user"},` +
						`{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"}],"summary":"2 findings"}`,
				}, nil
			}
			return &StepOutcome{
				NeedsApproval:   true,
				ReviewedPaths:   []string{"service.go", "new.go"},
				ReviewablePaths: []string{"service.go", "new.go"},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	added := []types.Finding{{
		ID:          "user-1",
		Severity:    types.FindingSeverityInfo,
		File:        "new.go",
		Description: "new user note",
		Action:      types.ActionNoOp,
	}}
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, nil, added, ""); err != nil {
		t.Fatalf("fix with colliding user finding: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("expected the unselected carried finding to remain outstanding")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	for _, item := range parsed.Items {
		if item.Description == "new user note" {
			t.Fatalf("reminted user finding was not tracked for verification: %s", *steps[0].FindingsJSON)
		}
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_ReviewCarryForward_PendingSelectionsSurviveLaterRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			round++
			switch round {
			case 1:
				return &StepOutcome{
					NeedsApproval: true,
					Findings: `{"findings":[` +
						`{"id":"review-1","severity":"error","file":"service.go","description":"nil deref","action":"ask-user"},` +
						`{"id":"review-2","severity":"warning","file":"cache.go","description":"unbounded cache","action":"ask-user"}],"summary":"2 findings"}`,
				}, nil
			case 2:
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-2","severity":"warning","file":"cache.go","description":"unbounded cache","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"cache.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			default:
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-2","severity":"warning","file":"cache.go","description":"unbounded cache","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			}
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	waitForRounds := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			steps, err := database.GetStepsByRun(run.ID)
			if err == nil && len(steps) > 0 {
				rounds, roundsErr := database.GetRoundsByStep(steps[0].ID)
				if roundsErr == nil && len(rounds) >= want && steps[0].Status == types.StepStatusFixReview {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("review did not reach round %d", want)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitForRounds(2)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}
	waitForRounds(3)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("finding B did not remain outstanding")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse outstanding findings: %v", err)
	}
	var hasA, hasB bool
	for _, item := range parsed.Items {
		hasA = hasA || item.ID == "review-1"
		hasB = hasB || item.ID == "review-2"
	}
	if hasA {
		t.Fatalf("finding A remained outstanding after a later positive verification: %s", *steps[0].FindingsJSON)
	}
	if !hasB {
		t.Fatalf("finding B was not preserved while A was verified: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_ReviewCarryForward_PositiveCoverageClearsFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			return &StepOutcome{ReviewedPaths: []string{"service.go"}, ReviewablePaths: []string{"service.go"}}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("step status = %s, want %s", steps[0].Status, types.StepStatusCompleted)
	}
	if steps[0].FindingsJSON != nil {
		t.Fatalf("verified finding still stored as outstanding: %s", *steps[0].FindingsJSON)
	}
	stats, err := database.StepFindingStats(steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if stats.FixedFindings != 1 {
		t.Errorf("stats reported %d fixed findings, want the one positively verified finding", stats.FixedFindings)
	}
}

// TestExecutor_ReviewCarryForward_AnOpenQuestionDoesNotBlockVerification pins
// the boundary between the two mechanisms. A review question is not a report
// about the code, so it must not take part in verification: a question's file
// is optional and the omission marker never has one, so leaving questions in
// the verification input made hasUnanchoredFinding true and refused to clear
// EVERY selected finding for as long as any question stayed open.
func TestExecutor_ReviewCarryForward_AnOpenQuestionDoesNotBlockVerification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			// The fix is verified - service.go was reviewed and reports
			// nothing - while an unanswered question rides along, exactly as
			// ReviewStep.Execute appends one to its own output.
			return &StepOutcome{
				NeedsApproval:   true,
				Findings:        `{"findings":[{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: keep the legacy route?","action":"ask-user","category":"review-question"}],"summary":"one open question"}`,
				ReviewedPaths:   []string{"service.go"},
				ReviewablePaths: []string{"service.go"},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("the gate carries no findings at all, so the open question was lost")
	}
	if strings.Contains(*steps[0].FindingsJSON, `"review-1"`) {
		t.Fatalf("the verified finding stayed outstanding because a question was open: %s", *steps[0].FindingsJSON)
	}
	if !strings.Contains(*steps[0].FindingsJSON, "question-q1") {
		t.Fatalf("the open question is missing from the gate: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_AnUnreadableHistoryDoesNotBlockVerification
// is the sibling of the test above for the marker the step emits when the
// reviewer's question history could not be read in full. It carries no File,
// so leaving it in the verification input sets hasUnanchoredFinding and refuses
// to clear ANY selected finding - and questions.ndjson is append-only, so the
// condition is permanent: a genuinely fixed finding stayed outstanding and the
// gate re-parked on it for ever. It carries no review-question category either,
// so the category-keyed drop does not reach it.
func TestExecutor_ReviewCarryForward_AnUnreadableHistoryDoesNotBlockVerification(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"ask-user"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			return &StepOutcome{
				NeedsApproval:   true,
				Findings:        `{"findings":[{"id":"review-questions-unreadable","severity":"warning","description":"The reviewer's question history could not be read in full. Decide this gate yourself.","action":"ask-user"}],"summary":"unreadable question history"}`,
				ReviewedPaths:   []string{"service.go"},
				ReviewablePaths: []string{"service.go"},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("the gate carries no findings at all, so the marker was lost")
	}
	if strings.Contains(*steps[0].FindingsJSON, `"review-1"`) {
		t.Fatalf("the verified finding stayed outstanding because the question history was unreadable: %s", *steps[0].FindingsJSON)
	}
	if !strings.Contains(*steps[0].FindingsJSON, "review-questions-unreadable") {
		t.Fatalf("the unreadable-history marker is missing from the gate: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_AnAnswerRoundWithdrawsByName covers the round
// type the append-only set was not written for. The reviewer's protocol tells
// it to prefix any finding contingent on an open question with
// "PENDING ANSWER (<id>)", so a round that asks a question routinely reports
// one, and the finalize turn that learns the answer disproves it must be able
// to retract it - otherwise the carry-forward re-injects it, still pointing at
// a question that is already settled.
//
// It retracts by NAMING the finding in withdrawn_findings. Clearing it by
// coverage silence was the earlier rule and is gone: an answer changes what the
// reviewer knows rather than the code, so a turn that covered the file proved
// nothing about any finding in it, and an unrelated finding used to drop with
// it. The sibling below pins that silence now keeps a finding.
func TestExecutor_ReviewCarryForward_AnAnswerRoundWithdrawsByName(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"warning","file":"service.go","line":10,"description":"PENDING ANSWER (q1): the legacy route is only wrong if /v1 is going","action":"ask-user"},{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: is /v1 going?","action":"ask-user","category":"review-question"}],"summary":"1 finding and a question"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			// The finalize turn: the answer settled the question and
			// disproved the contingent finding, so it says so by name.
			return &StepOutcome{
				ReviewedPaths:     []string{"service.go"},
				ReviewablePaths:   []string{"service.go"},
				WithdrawnFindings: []types.WithdrawnFinding{{ID: "review-1", Reason: "the answer settled it"}},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("step status = %s, want %s: the answer round could not withdraw the finding its answer disproved", steps[0].Status, types.StepStatusCompleted)
	}
	if steps[0].FindingsJSON != nil && outstandingIDs(t, *steps[0].FindingsJSON)["review-1"] {
		t.Fatalf("the contingent finding survived the answer that disproved it: %s", *steps[0].FindingsJSON)
	}
}

// TestExecutor_ReviewCarryForward_AnAnswerRoundSilenceKeepsAnUnrelatedFinding is
// the defect the withdrawal list exists to close.
//
// Under the earlier rule an answer round seeded its whole carried set as
// pending verification, so the finalize turn's coverage record cleared any
// carried finding whose file it named and did not re-report - including one the
// answers had nothing to do with. That let the review gate complete having
// silently dropped a defect nobody fixed, selected or approved. Here the turn
// covers the file and withdraws NOTHING, so the unrelated finding must survive.
func TestExecutor_ReviewCarryForward_AnAnswerRoundSilenceKeepsAnUnrelatedFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"warning","file":"service.go","line":10,"description":"PENDING ANSWER (q1): the legacy route is only wrong if /v1 is going","action":"ask-user"},{"id":"review-2","severity":"error","file":"service.go","line":88,"description":"nil deref on the error path, nothing to do with the question","action":"ask-user"},{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: is /v1 going?","action":"ask-user","category":"review-question"}],"summary":"2 findings and a question"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			// The finalize turn withdraws only what the answer disproved, and
			// says nothing about the unrelated finding, over a full coverage
			// record for the file both live in.
			return &StepOutcome{
				ReviewedPaths:     []string{"service.go"},
				ReviewablePaths:   []string{"service.go"},
				WithdrawnFindings: []types.WithdrawnFinding{{ID: "review-1", Reason: "the answer settled it"}},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	// Respond returns before the round runs, so the pre-answer snapshot is
	// still on the step. The finalize turn is the one that settles the
	// question, so its disappearance is the signal that the round landed.
	var steps []*db.StepResult
	deadline := time.Now().Add(20 * time.Second)
	for {
		var err error
		steps, err = database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) > 0 && steps[0].FindingsJSON != nil && !strings.Contains(*steps[0].FindingsJSON, "question-q1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the answer round never landed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("the gate lost every finding, including the unrelated one")
	}
	outstanding := outstandingIDs(t, *steps[0].FindingsJSON)
	if !outstanding["review-2"] {
		t.Fatalf("an unrelated finding was cleared by an answer round that never withdrew it: %s", *steps[0].FindingsJSON)
	}
	if outstanding["review-1"] {
		t.Fatalf("the withdrawn finding survived its own retraction: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// TestResolveVerifiedFindingsJSON pins the verify-before-clear rule: only a
// positive coverage record that also stops reporting the defect clears a
// selected finding. Silence, a round that looked elsewhere, a re-reported
// defect, and a finding with no file all leave it outstanding.
func TestResolveVerifiedFindingsJSON(t *testing.T) {
	// An EXACT restatement is what the round's own output must contain for the
	// item to read as still reported. Identity here is still content-derived, so
	// a reworded restatement does not match: it is appended as a new item by
	// mergeOutstandingFindingsJSON (the defect stays tracked, under a new ID).
	// Stable identity is a separate design pass (Parts 2+3 of the scout report).
	reported := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"nil deref on the error path","action":"ask-user"}],"summary":"1 finding"}`

	// lineShiftedReword is a fix that moved the defect to a different line in
	// the same file and a rereview that restated it under a different
	// description - matching neither the exact file+line key nor the content
	// fingerprint. This must never read as a clean pass: any finding reported
	// in the same file is ambiguous evidence, not positive verification.
	lineShiftedReword := `{"findings":[{"id":"review-9","severity":"info","file":"service.go","line":11,"description":"no remaining issue in this area","action":"no-op"}],"summary":"1 finding"}`

	cases := []struct {
		name        string
		thisRound   string
		reviewed    []string
		pending     []string
		wantCleared bool
	}{
		{name: "no coverage record clears nothing", thisRound: "", reviewed: nil, pending: []string{"review-1"}},
		{name: "empty coverage list clears nothing", thisRound: "", reviewed: []string{}, pending: []string{"review-1"}},
		{name: "coverage of another file clears nothing", thisRound: "", reviewed: []string{"cache.go"}, pending: []string{"review-1"}},
		{name: "out-of-scope coverage clears nothing", thisRound: "", reviewed: []string{"unrelated.go"}, pending: []string{"review-1"}},
		{name: "mixed in-scope and out-of-scope coverage clears nothing", thisRound: "", reviewed: []string{"service.go", "unrelated.go"}, pending: []string{"review-1"}},
		{name: "reported defect stays outstanding", thisRound: reported, reviewed: []string{"service.go"}, pending: []string{"review-1"}},
		{name: "unanchored current finding prevents clearing", thisRound: `{"findings":[{"id":"review-9","severity":"info","description":"unanchored observation","action":"no-op"}],"summary":"1 finding"}`, reviewed: []string{"service.go"}, pending: []string{"review-1"}},
		{name: "finding covered and no longer reported clears", thisRound: "", reviewed: []string{"service.go"}, pending: []string{"review-1"}, wantCleared: true},
		{name: "an unwatched finding keeps its neighbour pending", thisRound: "", reviewed: []string{"cache.go"}, pending: []string{"review-1"}},
		{name: "line-shifted reword in the same file is ambiguous, not resolution", thisRound: lineShiftedReword, reviewed: []string{"service.go"}, pending: []string{"review-1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVerifiedFindingsJSON(reviewCarryTwoFindings, tc.pending, tc.reviewed, []string{"service.go", "cache.go"}, tc.thisRound)
			parsed, err := types.ParseFindingsJSON(got)
			if err != nil {
				t.Fatalf("parse result: %v", err)
			}
			cleared := true
			for _, item := range parsed.Items {
				if item.ID == "review-1" {
					cleared = false
				}
			}
			if cleared != tc.wantCleared {
				t.Fatalf("review-1 cleared = %v, want %v (result: %s)", cleared, tc.wantCleared, got)
			}
		})
	}
}

// TestResolveVerifiedFindingsJSON_FilelessPendingFinding pins the upgrade
// compat carve-out for findings the reviewer could not anchor to a file: no
// coverage record can ever match them, so a SELECTED file-less item clears on
// a positive verification round that covers every reviewable path and no
// longer reports it - and stays on partial coverage, an unanchored rereport,
// or a round that is not a positive verification. Runs parked before the recorded-decision review machinery was
// removed carry such items, and a fix selection could otherwise never clear
// them.
func TestResolveVerifiedFindingsJSON_FilelessPendingFinding(t *testing.T) {
	outstanding := `{"findings":[{"id":"review-1","severity":"warning","description":"finding with no file anchor","action":"ask-user"}],"summary":"1 finding"}`
	reported := `{"findings":[{"id":"review-9","severity":"warning","description":"finding with no file anchor","action":"ask-user"}],"summary":"1 finding"}`
	unrelatedUnanchored := `{"findings":[{"id":"review-9","severity":"info","description":"unanchored observation","action":"no-op"}],"summary":"1 finding"}`
	unrelatedAnchored := `{"findings":[{"id":"review-9","severity":"info","file":"service.go","line":5,"description":"unrelated anchored observation","action":"no-op"}],"summary":"1 finding"}`

	cases := []struct {
		name        string
		thisRound   string
		reviewed    []string
		pending     []string
		wantCleared bool
	}{
		{name: "not selected stays outstanding", thisRound: "", reviewed: []string{"service.go", "cache.go"}, pending: nil},
		{name: "no coverage record stays outstanding", thisRound: "", reviewed: nil, pending: []string{"review-1"}},
		{name: "out-of-scope coverage stays outstanding", thisRound: "", reviewed: []string{"unrelated.go"}, pending: []string{"review-1"}},
		{name: "partial coverage not reporting it stays outstanding", thisRound: "", reviewed: []string{"service.go"}, pending: []string{"review-1"}},
		{name: "full coverage not reporting it clears", thisRound: "", reviewed: []string{"service.go", "cache.go"}, pending: []string{"review-1"}, wantCleared: true},
		{name: "unrelated anchored finding still clears", thisRound: unrelatedAnchored, reviewed: []string{"service.go", "cache.go"}, pending: []string{"review-1"}, wantCleared: true},
		{name: "re-reported under a new id stays outstanding", thisRound: reported, reviewed: []string{"service.go", "cache.go"}, pending: []string{"review-1"}},
		{name: "unrelated unanchored finding stays outstanding", thisRound: unrelatedUnanchored, reviewed: []string{"service.go", "cache.go"}, pending: []string{"review-1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVerifiedFindingsJSON(outstanding, tc.pending, tc.reviewed, []string{"service.go", "cache.go"}, tc.thisRound)
			if tc.wantCleared {
				if got != "" {
					parsed, err := types.ParseFindingsJSON(got)
					if err == nil {
						for _, item := range parsed.Items {
							if item.ID == "review-1" {
								t.Fatalf("selected file-less finding survived a positive verification that did not report it: %s", got)
							}
						}
					}
				}
				return
			}
			if !strings.Contains(got, "review-1") {
				t.Fatalf("file-less finding was verified away: %s", got)
			}
		})
	}
}

func TestRemapFindingIDsJSON_UsesRemintedAutomaticSelection(t *testing.T) {
	merged := mergeOutstandingFindingsJSON(reviewCarryTwoFindings, `{"findings":[{"id":"review-1","severity":"error","file":"other.go","description":"new automatic defect","action":"auto-fix"}],"summary":"1 finding"}`, nil)
	selected := autoFixableFindingsJSON(`{"findings":[{"id":"review-1","severity":"error","file":"other.go","description":"new automatic defect","action":"auto-fix"}],"summary":"1 finding"}`)
	remapped := remapFindingIDsJSON(merged, selected)
	parsed, err := types.ParseFindingsJSON(remapped)
	if err != nil {
		t.Fatalf("parse remapped findings: %v", err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].ID != "review-3" {
		t.Fatalf("automatic selection ID = %q, want reminted review-3: %s", parsed.Items[0].ID, remapped)
	}
}

// TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity pins the
// append-only merge: the accumulated set is never reduced, an item keeps its
// ID (the selector `axi respond --findings <id>` uses), and a colliding new ID
// is re-minted rather than silently replacing the outstanding item.
func TestMergeOutstandingFindingsJSON_UsesCurrentReviewedPathsWhenRoundIsEmpty(t *testing.T) {
	prior := `{"findings":[` +
		`{"id":"review-1","severity":"error","file":"service.go","description":"old issue","action":"ask-user"},` +
		`{"id":"review-2","severity":"warning","file":"cache.go","description":"still outstanding","action":"ask-user"}],` +
		`"reviewed_paths":["old.go"]}`

	merged := mergeOutstandingFindingsJSON(prior, "", []string{"current.go"})
	parsed, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("merged findings = %d, want 2", len(parsed.Items))
	}
	if len(parsed.ReviewedPaths) != 1 || parsed.ReviewedPaths[0] != "current.go" {
		t.Fatalf("reviewed paths = %v, want [current.go]", parsed.ReviewedPaths)
	}
}

func TestMergeOutstandingFindingsJSON_AppendsAndKeepsSelectionIdentity(t *testing.T) {
	merged := mergeOutstandingFindingsJSON(reviewCarryTwoFindings, `{"findings":[{"id":"review-1","severity":"info","file":"other.go","description":"restated as a new item","action":"ask-user"}],"summary":"1 finding"}`, nil)
	if !strings.Contains(merged, "service.go") || !strings.Contains(merged, "cache.go") {
		t.Fatalf("append-only merge dropped an outstanding finding: %s", merged)
	}
	parsed, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	seen := map[string]int{}
	for _, item := range parsed.Items {
		seen[item.ID]++
	}
	if seen["review-1"] != 1 {
		t.Fatalf("review-1 appears %d times, want exactly one: %s", seen["review-1"], merged)
	}
	var kept *types.Finding
	for i := range parsed.Items {
		if parsed.Items[i].ID == "review-1" {
			kept = &parsed.Items[i]
		}
	}
	if kept == nil || kept.File != "service.go" {
		t.Fatalf("the outstanding item's identity was replaced by the colliding new one: %s", merged)
	}
}

// TestRetainFindingIDsByIdentity_RejectsPositionalIDReuse closes the Greptile
// P1 that recovery unions selected IDs from every historical round and then
// retains any ID present in the LATEST findings, with no check that it is
// the SAME finding. Finding IDs are positional and get re-minted for an
// unrelated finding once the original drops out of the outstanding set
// (mergeOutstandingFindingsJSON hands a freed ID to the next new item). An
// old selection under a reused ID must not silently alias the new,
// never-selected finding as "selected".
func TestRetainFindingIDsByIdentity_RejectsPositionalIDReuse(t *testing.T) {
	roundOneFindings := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"old carried issue","action":"ask-user"}],"summary":"1 finding"}`
	roundOne := &db.StepRound{FindingsJSON: strPtr(roundOneFindings), SelectedFindingIDs: strPtr(`["review-1"]`)}

	// The old finding resolved and dropped; a LATER, completely unrelated
	// finding happens to be re-minted under the same freed ID and was never
	// selected by the operator.
	latestFindings := `{"findings":[{"id":"review-1","severity":"info","file":"unrelated.go","description":"a different, never-selected finding","action":"auto-fix"}],"summary":"1 finding"}`
	roundTwo := &db.StepRound{FindingsJSON: strPtr(latestFindings), SelectedFindingIDs: nil}

	rounds := []*db.StepRound{roundOne, roundTwo}
	selected := combineFindingIDLists(nil, findingIDsFromSelectionJSON(*roundOne.SelectedFindingIDs))

	// The unguarded helper only checks presence, so it wrongly keeps the ID -
	// pinning why the identity-aware guard is required at all.
	if plain := retainFindingIDs(latestFindings, selected); len(plain) != 1 || plain[0] != "review-1" {
		t.Fatalf("retainFindingIDs (presence-only) = %v, want [review-1] to demonstrate the aliasing hazard it does not guard against", plain)
	}

	identity := selectedFindingIdentities(rounds)
	got := retainFindingIDsByIdentity(latestFindings, selected, identity)
	if len(got) != 0 {
		t.Fatalf("retainFindingIDsByIdentity() = %v, want empty: the reused ID must not alias the unrelated new finding as selected", got)
	}
}

func TestRetainFindingIDsByIdentity_UsesUserFindingIdentity(t *testing.T) {
	userFindings := `{"findings":[{"id":"user-1","severity":"warning","file":"old.go","description":"selected user finding","action":"auto-fix","source":"user"}],"summary":"1 finding"}`
	round := &db.StepRound{
		UserFindingsJSON:   strPtr(userFindings),
		SelectedFindingIDs: strPtr(`["user-1"]`),
	}
	latestFindings := `{"findings":[{"id":"user-1","severity":"info","file":"new.go","description":"unrelated later finding","action":"no-op"}],"summary":"1 finding"}`

	identity := selectedFindingIdentities([]*db.StepRound{round})
	if _, ok := identity["user-1"]; !ok {
		t.Fatal("selected user finding identity was not recovered from user_findings_json")
	}
	got := retainFindingIDsByIdentity(latestFindings, []string{"user-1"}, identity)
	if len(got) != 0 {
		t.Fatalf("retainFindingIDsByIdentity() = %v, want empty for reused user finding ID", got)
	}
}

// TestRetainFindingIDsByIdentity_KeepsGenuineSameFindingAcrossRounds proves
// the identity guard does not over-block: an ID that still names the SAME
// finding across rounds must remain retained.
func TestRetainFindingIDsByIdentity_KeepsRemappedUserFindingAfterRecovery(t *testing.T) {
	persistedUserFindings := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"},{"id":"review-2","severity":"info","file":"new.go","description":"new user note","action":"no-op","source":"user"}],"summary":"2 findings"}`
	latestFindings := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"},{"id":"review-2","severity":"info","file":"new.go","description":"new user note","action":"no-op","source":"user"},{"id":"review-3","severity":"warning","file":"other.go","description":"different finding","action":"auto-fix"}],"summary":"3 findings"}`
	roundOne := &db.StepRound{
		FindingsJSON:       strPtr(`{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"selected issue","action":"ask-user"}],"summary":"1 finding"}`),
		UserFindingsJSON:   strPtr(persistedUserFindings),
		SelectedFindingIDs: strPtr(`["review-1","review-2"]`),
	}
	roundTwo := &db.StepRound{
		FindingsJSON:       strPtr(latestFindings),
		SelectedFindingIDs: strPtr(`["review-3"]`),
	}

	identity := selectedFindingIdentities([]*db.StepRound{roundOne, roundTwo})
	got := retainFindingIDsByIdentity(latestFindings, []string{"review-2"}, identity)
	if len(got) != 1 || got[0] != "review-2" {
		t.Fatalf("retainFindingIDsByIdentity() = %v, want remapped review-2 retained after recovery", got)
	}
}

func TestRetainFindingIDsByIdentity_KeepsGenuineSameFindingAcrossRounds(t *testing.T) {
	roundOneFindings := `{"findings":[{"id":"review-1","severity":"error","file":"service.go","description":"old carried issue","action":"ask-user"}],"summary":"1 finding"}`
	roundOne := &db.StepRound{FindingsJSON: strPtr(roundOneFindings), SelectedFindingIDs: strPtr(`["review-1"]`)}
	// Same finding, still outstanding in a later round under the same ID.
	roundTwo := &db.StepRound{FindingsJSON: strPtr(roundOneFindings), SelectedFindingIDs: nil}

	rounds := []*db.StepRound{roundOne, roundTwo}
	selected := combineFindingIDLists(nil, findingIDsFromSelectionJSON(*roundOne.SelectedFindingIDs))
	identity := selectedFindingIdentities(rounds)
	got := retainFindingIDsByIdentity(roundOneFindings, selected, identity)
	if len(got) != 1 || got[0] != "review-1" {
		t.Fatalf("retainFindingIDsByIdentity() = %v, want [review-1] retained for the genuinely same finding", got)
	}
}

func strPtr(v string) *string { return &v }

// TestExecutor_ReviewCarryForward_AFixRoundCannotWithdraw pins the retraction
// list to the round type it was written for.
//
// withdrawn_findings lives in the findings schema every review turn shares, so
// a fix-round rereview is offered the field too - and the schema's "answer
// rounds only" description is guidance to the agent, never enforcement. A fix
// round that claimed it would clear a selected finding with no coverage record
// at all: exactly the no-op fix TestExecutor_ReviewCarryForward_NoOpFixKeepsFindingParked
// exists to catch, taking a different route out of the outstanding set. Only a
// finalize turn may retract; every other round stays on the coverage rule.
func TestExecutor_ReviewCarryForward_AFixRoundCannotWithdraw(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        reviewCarryTwoFindings,
					ReviewedPaths:   []string{"service.go", "cache.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			}
			if err := os.WriteFile(filepath.Join(workDir, "unrelated.txt"), []byte("tidy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			execGit(t, workDir, "add", "unrelated.txt")
			execGit(t, workDir, "commit", "-m", "tidy unrelated code")
			// The rereview never looked at service.go, so it has no coverage
			// record to clear the selected finding with - and tries to retract
			// it by name instead.
			return &StepOutcome{
				FixSummary:        "tidy unrelated code",
				WithdrawnFindings: []types.WithdrawnFinding{{ID: "review-1", Reason: "the answer settled it"}},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("the outstanding set is empty: a fix round's retraction cleared a finding nothing verified")
	}
	if !strings.Contains(*steps[0].FindingsJSON, "review-1") {
		t.Fatalf("a fix round withdrew a selected finding no round verified: %s", *steps[0].FindingsJSON)
	}

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status == types.RunCompleted {
		t.Fatal("run completed over a finding a fix round retracted without verifying")
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_AnAnswerRoundDoesNotCarryItsOwnQuestions
// drives the carried set the executor really produces, which is what the
// step-level fixtures cannot: they hand-write a code finding alone, while the
// set taken at the ActionAnswer branch is the outstanding set BEFORE the next
// round drops question rows from it - so it always still holds the
// question-<id> row whose emission is why the gate parked.
//
// Handing that row to carriedFindingsPromptSection tells the finalize turn to
// re-adjudicate a finding the step generated rather than one the reviewer made,
// and a turn that complies echoes it back through a findings schema that has no
// category field. The echo is therefore uncategorised: dropReviewQuestionFindingsJSON
// (keyed on the category and the unreadable-marker ID, never the "question-"
// prefix) never removes it, it re-parks the gate as an ordinary ask-user
// warning, HasUnansweredReviewQuestion is false for it so --yes and the TUI's
// yolo hand the fixer a question row, and the `axi answer` its own description
// instructs records a duplicate that releases nothing.
func TestExecutor_ReviewCarryForward_AnAnswerRoundDoesNotCarryItsOwnQuestions(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round := 0
	var carried string
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"review-1","severity":"warning","file":"service.go","line":10,"description":"PENDING ANSWER (q1): the legacy route is only wrong if /v1 is going","action":"ask-user"},{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: is /v1 going? Answer it with: no-mistakes axi answer --question q1 --answer ...","action":"ask-user","category":"review-question"}],"summary":"1 finding and a question"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			carried = sctx.CarriedFindings
			return &StepOutcome{
				ReviewedPaths:     []string{"service.go"},
				ReviewablePaths:   []string{"service.go"},
				WithdrawnFindings: []types.WithdrawnFinding{{ID: "review-1", Reason: "the answer settled it"}},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)

	if round < 2 {
		t.Fatalf("the finalize turn never ran (round = %d)", round)
	}
	parsed, err := types.ParseFindingsJSON(carried)
	if err != nil {
		t.Fatalf("parse the carried set: %v (%q)", err, carried)
	}
	var sawCode bool
	for _, item := range parsed.Items {
		if item.Category == types.FindingCategoryReviewQuestion {
			t.Errorf("the finalize turn was asked to re-adjudicate its own question row %q; an echo of it comes back uncategorised and parks the gate for ever", item.ID)
		}
		if item.ID == "review-1" {
			sawCode = true
		}
	}
	if !sawCode {
		t.Fatalf("the reviewer's own code finding was dropped from the carried set, so the turn cannot re-adjudicate it: %q", carried)
	}
}

// TestExecutor_ReviewCarryForward_AnAnswerRoundCannotWithdrawTheOperatorsOwnFinding
// covers the one finding a retraction may never reach. `respond --action fix
// --add-finding` stamps the item Source=user with a user-N id, and that id and
// source both survive into the outstanding set - so an answer round's carried
// set hands it to the finalize turn like any other row. It is not a claim the
// reviewer made, it is an instruction the operator gave, so a turn naming it in
// withdrawn_findings must change nothing: it leaves only on positive coverage
// or on the operator's own approve, skip or abort, which is the contract
// docs/.../pipeline-steps.md already states for a selected finding.
func TestExecutor_ReviewCarryForward_AnAnswerRoundCannotWithdrawTheOperatorsOwnFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        reviewCarryTwoFindings,
					ReviewedPaths:   []string{"service.go", "cache.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			}
			if round == 2 {
				// The fix round's rereview offers no coverage record, so
				// nothing clears the ordinary way and the gate re-parks with
				// the operator's own finding still outstanding.
				return &StepOutcome{FixSummary: "no change"}, nil
			}
			// The finalize turn tries to retract the operator's instruction.
			// It reports no coverage either, so the withdrawal is the only
			// route by which user-1 could leave. The extra finding is how the
			// test sees this round land: the gate is already parked as
			// fix_review from the round before, so the status alone would let
			// it read a pre-answer snapshot and pass vacuously.
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"id":"review-9","severity":"info","file":"service.go","line":1,"description":"finalize turn ran","action":"no-op"}],"summary":"finalize"}`,
				WithdrawnFindings: []types.WithdrawnFinding{
					{ID: "user-1", Reason: "the answer says this is intended"},
				},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, nil, []types.Finding{{
		Severity:    types.FindingSeverityError,
		File:        "service.go",
		Line:        10,
		Description: "the operator's own instruction: keep the /v1 shim",
		Action:      types.ActionAskUser,
	}}, ""); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("the gate carries no findings at all")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse gate findings: %v", err)
	}
	var operatorID string
	for _, item := range parsed.Items {
		if item.Source == types.FindingSourceUser {
			operatorID = item.ID
		}
	}
	if operatorID != "user-1" {
		t.Fatalf("fixture did not produce the operator-authored user-1 finding: %s", *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	// The gate is already fix_review, so the status cannot say the answer round
	// landed; its own finding is what does.
	deadline := time.Now().Add(20 * time.Second)
	for {
		steps, err = database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) > 0 && steps[0].FindingsJSON != nil && strings.Contains(*steps[0].FindingsJSON, "finalize turn ran") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the answer round never landed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Parsed, not substring-matched: an applied retraction records the id it
	// removed on the round, so the id is present in the payload either way.
	after, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse gate findings: %v", err)
	}
	var stillOutstanding bool
	for _, item := range after.Items {
		if item.ID == "user-1" {
			stillOutstanding = true
		}
	}
	if !stillOutstanding {
		t.Fatalf("the reviewer retracted the operator's own finding; it may leave only on coverage or the operator's verdict: %s", *steps[0].FindingsJSON)
	}
	for _, w := range after.WithdrawnFindings {
		if w.ID == "user-1" {
			t.Fatalf("a refused retraction was recorded as if it had been applied: %s", *steps[0].FindingsJSON)
		}
	}

	// The same id still clears the ordinary way: the operator's own verdict.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// outstandingIDs is the ids still OUTSTANDING in a persisted findings payload.
// A round that applied a retraction also records the ids it removed on that
// payload, so a substring match over the raw JSON cannot tell a finding that
// survived from one the round reported retracting.
func outstandingIDs(t *testing.T, raw string) map[string]bool {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	ids := make(map[string]bool, len(parsed.Items))
	for _, item := range parsed.Items {
		ids[item.ID] = true
	}
	return ids
}

// withdrawnIDs is the retraction record a persisted round carries. It is the
// round's own claim about what IT removed, so a round that retracted nothing
// must carry none.
func withdrawnIDs(t *testing.T, raw string) []string {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	ids := make([]string, 0, len(parsed.WithdrawnFindings))
	for _, w := range parsed.WithdrawnFindings {
		ids = append(ids, w.ID)
	}
	return ids
}

// TestExecutor_ReviewCarryForward_ARetractionIsRecordedOnlyOnTheRoundThatMadeIt
// covers the stamp-after-assign ordering. An answer round's retraction is
// stamped onto the payload it persists, and a later round that retracted
// nothing must carry none - which holds here because the loop takes the
// outstanding set it carries forward BEFORE the stamp, so the record cannot
// ride along with it.
//
// It does not reach the merge-inheritance route: both of its resolutions are
// answers, and only an operator `--action fix` after a retracting answer round
// merges from the stamped payload. That route is guarded by the
// WithdrawnFindings resets in mergeOutstandingFindingsJSON, covered by
// TestExecutor_ReviewCarryForward_AnEmptyOutstandingSetRecordsNoRetraction and
// TestExecutor_ReviewCarryForward_ARecoveredRoundInheritsNoRetractionRecord.
func TestExecutor_ReviewCarryForward_ARetractionIsRecordedOnlyOnTheRoundThatMadeIt(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			switch round {
			case 1:
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        reviewCarryTwoFindings,
					ReviewedPaths:   []string{"service.go", "cache.go"},
					ReviewablePaths: []string{"service.go", "cache.go"},
				}, nil
			case 2:
				// The answer round retracts review-1 and re-parks on a finding
				// of its own, so the record can be read before the next round.
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-3","severity":"warning","file":"cache.go","line":7,"description":"answer round finding","action":"ask-user"}],"summary":"answer"}`,
					WithdrawnFindings: []types.WithdrawnFinding{
						{ID: "review-1", Reason: "the answer settled it"},
					},
				}, nil
			default:
				// Reports nothing at all, so the outstanding set is rebuilt
				// from itself - the merge's other branch, where an inherited
				// record would ride along just the same.
				return &StepOutcome{NeedsApproval: true, FixSummary: "silent round"}, nil
			}
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	steps := waitForFindings(t, database, run.ID, "answer round finding")
	if got := withdrawnIDs(t, *steps[0].FindingsJSON); len(got) != 1 || got[0] != "review-1" {
		t.Fatalf("the answer round did not record the retraction it made: %v (%s)", got, *steps[0].FindingsJSON)
	}

	// The very next round inherits the outstanding set from that stamped
	// payload. A second answer - the reviewer asked again and the operator
	// answered again - dispatches no fix, so nothing re-merges the set on the
	// way in; the round then reports nothing of its own, taking the merge
	// branch that rebuilds the set from itself. Its row must carry no
	// retraction: it made none.
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	rounds := waitForRounds(t, database, steps[0].ID, 3)
	last := rounds[len(rounds)-1]
	if last.FindingsJSON == nil {
		t.Fatal("the silent round persisted no findings at all")
	}
	if got := withdrawnIDs(t, *last.FindingsJSON); len(got) != 0 {
		t.Fatalf("a round that retracted nothing recorded %v; the record belongs to the round that made it: %s", got, *last.FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// TestExecutor_ReviewCarryForward_AnEmptyOutstandingSetRecordsNoRetraction
// covers the second route, which the operator-finding regression cannot reach
// because its outstanding set is never empty. When the gate carries only
// question rows - the common case where the reviewer asks before it has
// reported any code finding - the outstanding set is empty after the question
// rows are dropped, so nothing is retractable and the executor applies nothing.
// The agent's own unfiltered withdrawn_findings must not ride its payload into
// the record as though the executor had applied it.
func TestExecutor_ReviewCarryForward_AnEmptyOutstandingSetRecordsNoRetraction(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	round := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			round++
			if round == 1 {
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"question-q1","severity":"warning","description":"Review question awaiting an answer: is /v1 going?","action":"ask-user","category":"review-question"}],"summary":"a question"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			// The finalize turn names an id that was never outstanding. Its
			// payload carries the agent's own withdrawn_findings verbatim.
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"id":"review-1","severity":"warning","file":"service.go","line":3,"description":"finalize turn finding","action":"ask-user"}],"summary":"finalize","withdrawn_findings":[{"id":"ghost-1","reason":"never outstanding"}]}`,
				WithdrawnFindings: []types.WithdrawnFinding{
					{ID: "ghost-1", Reason: "never outstanding"},
				},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	steps := waitForFindings(t, database, run.ID, "finalize turn finding")
	if got := withdrawnIDs(t, *steps[0].FindingsJSON); len(got) != 0 {
		t.Fatalf("a retraction the executor never applied was recorded as if it had been: %v (%s)", got, *steps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// waitForFindings blocks until the step's persisted findings carry marker. The
// gate status alone cannot say a round landed when consecutive rounds park the
// same way, which is how an earlier version of these tests read a pre-round
// snapshot and passed vacuously.
func waitForFindings(t *testing.T, database *db.DB, runID, marker string) []*db.StepResult {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		steps, err := database.GetStepsByRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) > 0 && steps[0].FindingsJSON != nil && strings.Contains(*steps[0].FindingsJSON, marker) {
			return steps
		}
		if time.Now().After(deadline) {
			t.Fatalf("the round carrying %q never landed", marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForRounds blocks until the step has persisted at least want rounds. A
// round that reports nothing changes no marker, so its row is the only place
// its arrival is visible.
func waitForRounds(t *testing.T, database *db.DB, stepResultID string, want int) []*db.StepRound {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rounds, err := database.GetRoundsByStep(stepResultID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) >= want {
			return rounds
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d rounds landed", len(rounds), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExecutor_ReviewCarryForward_ARecoveredRoundInheritsNoRetractionRecord
// covers the branch the live loop cannot reach. In-process, the outstanding set
// is taken BEFORE the retraction record is stamped, so only the persisted
// payload carries it - but recovery rebuilds the outstanding set FROM that
// persisted payload, so a recovered round starts holding a record it did not
// make. A round that then reports nothing of its own rebuilds the set from
// itself, and without a per-round reset it would persist that inherited
// record as its own.
func TestExecutor_ReviewCarryForward_ARecoveredRoundInheritsNoRetractionRecord(t *testing.T) {
	database, p, run, repo := setupTest(t)
	// Seeded exactly as a retracting answer round leaves the gate.
	findings := `{"findings":[{"id":"review-2","severity":"warning","file":"cache.go","line":42,"description":"unbounded cache growth","action":"ask-user"}],"summary":"1 finding","withdrawn_findings":[{"id":"review-1","reason":"the answer settled it"}]}`
	stepResult, recoveredRun := seedRecoveredReviewGate(t, database, run, findings, types.StepStatusAwaitingApproval, "")

	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, FixSummary: "silent recovered round"}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if respondErr = exec.Respond(types.StepReview, types.ActionAnswer, nil); respondErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond to recovered review: %v", respondErr)
	}

	rounds := waitForRounds(t, database, stepResult.ID, 2)
	last := rounds[len(rounds)-1]
	if last.FindingsJSON == nil {
		t.Fatal("the recovered round persisted no findings at all")
	}
	if got := withdrawnIDs(t, *last.FindingsJSON); len(got) != 0 {
		t.Fatalf("a recovered round that retracted nothing inherited the record %v: %s", got, *last.FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}
