package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// TestResolveVerifiedFindingsJSON_FilelessFindingIsNeverVerifiedAway pins the
// other half of the same rule for findings the reviewer could not anchor to a
// file: no coverage record can ever match them, so only an operator action
// clears them.
func TestResolveVerifiedFindingsJSON_FilelessFindingIsNeverVerifiedAway(t *testing.T) {
	outstanding := `{"findings":[{"id":"review-1","severity":"warning","description":"finding with no file anchor","action":"ask-user"}],"summary":"1 finding"}`
	got := resolveVerifiedFindingsJSON(outstanding, []string{"review-1"}, []string{"service.go", "cache.go"}, []string{"service.go", "cache.go"}, "")
	if !strings.Contains(got, "review-1") {
		t.Fatalf("file-less finding was verified away by an unrelated coverage record: %s", got)
	}
}

func TestExecutor_ReviewCarryForward_DecisionAssessmentClearsSelectedDecisionFinding(t *testing.T) {
	for _, tc := range []struct {
		name           string
		file           string
		reviewedPaths  []string
		reviewablePath []string
	}{
		{name: "ignored file", file: "ignored.go", reviewedPaths: []string{"current.go"}, reviewablePath: []string{"current.go"}},
		{name: "file absent from current diff", file: "removed-from-diff.go", reviewedPaths: []string{"current.go"}, reviewablePath: []string{"current.go"}},
		{name: "no file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			decisionID := "review/round-1/decision"
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				calls++
				if calls == 1 {
					findings := types.Findings{Items: []types.Finding{{
						DecisionID: decisionID, Severity: "warning", File: tc.file, Description: "recorded decision is contradicted",
						Action: types.ActionAskUser,
					}}, Summary: "recorded decision is contradicted"}
					raw, err := json.Marshal(findings)
					if err != nil {
						t.Fatal(err)
					}
					return &StepOutcome{NeedsApproval: true, Findings: string(raw)}, nil
				}
				verified, err := json.Marshal(types.Findings{
					DecisionReviews: []types.DecisionReview{{DecisionID: decisionID, Result: "satisfied", Evidence: "current tree preserves the selected behavior"}},
					Items:           []types.Finding{}, Summary: "no findings",
				})
				if err != nil {
					t.Fatal(err)
				}
				return &StepOutcome{Findings: string(verified), ReviewedPaths: tc.reviewedPaths, ReviewablePaths: tc.reviewablePath}, nil
			}}

			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || len(steps) != 1 || steps[0].FindingsJSON == nil {
				t.Fatalf("load parked decision finding: steps=%d err=%v", len(steps), err)
			}
			parked, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
			if err != nil || len(parked.Items) != 1 {
				t.Fatalf("parse parked decision finding: %+v, %v", parked, err)
			}
			if parked.Items[0].DecisionID != decisionID {
				t.Fatalf("parked decision identity = %q, want %q", parked.Items[0].DecisionID, decisionID)
			}
			if err := exec.Respond(types.StepReview, types.ActionFix, []string{parked.Items[0].ID}); err != nil {
				t.Fatal(err)
			}
			waitExecutorDone(t, done)
			if calls != 2 {
				t.Fatalf("review calls = %d, want 2", calls)
			}
		})
	}
}

func TestResolveVerifiedFindingsJSON_DecisionAssessmentsFailClosed(t *testing.T) {
	outstanding := `{"findings":[{"id":"review-1","decision_id":"decision-1","severity":"warning","description":"recorded decision contradicted","action":"ask-user"}],"summary":"blocked"}`
	valid := types.DecisionReview{DecisionID: "decision-1", Result: "satisfied", Evidence: "source-backed evidence"}
	for _, tc := range []struct {
		name    string
		reviews []types.DecisionReview
	}{
		{name: "missing"},
		{name: "duplicate", reviews: []types.DecisionReview{valid, valid}},
		{name: "blank identity", reviews: []types.DecisionReview{{DecisionID: "", Result: "satisfied", Evidence: "evidence"}}},
		{name: "blank evidence", reviews: []types.DecisionReview{{DecisionID: "decision-1", Result: "satisfied", Evidence: " "}}},
		{name: "malformed result", reviews: []types.DecisionReview{{DecisionID: "decision-1", Result: "approved", Evidence: "evidence"}}},
		{name: "adverse", reviews: []types.DecisionReview{{DecisionID: "decision-1", Result: "contradicted", Evidence: "contrary source"}}},
		{name: "unrelated", reviews: []types.DecisionReview{{DecisionID: "decision-2", Result: "satisfied", Evidence: "other decision"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(types.Findings{DecisionReviews: tc.reviews, Items: []types.Finding{}, Summary: "round"})
			if err != nil {
				t.Fatal(err)
			}
			got := resolveVerifiedFindingsJSON(outstanding, []string{"review-1"}, nil, nil, string(raw))
			if !strings.Contains(got, "review-1") {
				t.Fatalf("invalid assessment cleared the decision finding: %s", got)
			}
		})
	}
}

func TestResolveVerifiedFindingsJSON_DecisionIdentityCollisionFailsClosed(t *testing.T) {
	outstanding := `{"findings":[{"id":"review-1","decision_id":"decision-1","severity":"warning","description":"first decision finding","action":"ask-user"},{"id":"review-2","decision_id":"decision-1","severity":"warning","description":"colliding decision finding","action":"ask-user"}],"summary":"blocked"}`
	raw, err := json.Marshal(types.Findings{
		DecisionReviews: []types.DecisionReview{{DecisionID: "decision-1", Result: "satisfied", Evidence: "source-backed evidence"}},
		Items:           []types.Finding{}, Summary: "round",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := resolveVerifiedFindingsJSON(outstanding, []string{"review-1"}, nil, nil, string(raw))
	if !strings.Contains(got, "review-1") {
		t.Fatalf("colliding decision identity cleared a selected finding: %s", got)
	}
}

func TestExecutor_ReviewCarryForward_PersistsCurrentDecisionReviews(t *testing.T) {
	database, p, run, repo := setupTest(t)
	decisionID := "review/round-1/decision"
	newDecisionID := "test/round-2/decision"
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return &StepOutcome{NeedsApproval: true, Findings: `{"decision_reviews":[{"decision_id":"` + decisionID + `","result":"contradicted","evidence":"old evidence"}],"findings":[{"id":"review-1","decision_id":"` + decisionID + `","severity":"warning","description":"old decision finding","action":"ask-user"},{"id":"review-2","severity":"warning","file":"other.go","description":"ordinary outstanding finding","action":"ask-user"}],"summary":"blocked"}`}, nil
		}
		return &StepOutcome{NeedsApproval: true, Findings: `{"decision_reviews":[{"decision_id":"` + decisionID + `","result":"satisfied","evidence":"fresh evidence"},{"decision_id":"` + newDecisionID + `","result":"contradicted","evidence":"new decision evidence"}],"findings":[{"id":"review-1","decision_id":"` + newDecisionID + `","severity":"warning","description":"new decision finding","action":"ask-user"}],"summary":"still blocked"}`}, nil
	}}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 || steps[0].FindingsJSON == nil {
		t.Fatalf("load first round: steps=%d err=%v", len(steps), err)
	}
	first, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil || len(first.Items) < 1 {
		t.Fatalf("parse first round: %+v, %v", first, err)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	steps, err = database.GetStepsByRun(run.ID)
	if err != nil || steps[0].FindingsJSON == nil {
		t.Fatalf("load second round: %v", err)
	}
	current, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	want := []types.DecisionReview{{DecisionID: decisionID, Result: "satisfied", Evidence: "fresh evidence"}, {DecisionID: newDecisionID, Result: "contradicted", Evidence: "new decision evidence"}}
	if !reflect.DeepEqual(current.DecisionReviews, want) {
		t.Fatalf("decision reviews = %+v, want current round %+v", current.DecisionReviews, want)
	}
	if !slices.ContainsFunc(current.Items, func(item types.Finding) bool { return item.Description == "ordinary outstanding finding" }) {
		t.Fatalf("ordinary outstanding finding was lost: %+v", current.Items)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_ReviewCarryForward_RewordedAdverseDecisionCanLaterClear(t *testing.T) {
	database, p, run, repo := setupTest(t)
	decisionID := "review/round-1/decision"
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		result := "contradicted"
		evidence := "first adverse explanation"
		description := "first wording"
		if calls == 2 {
			evidence = "different adverse explanation"
			description = "reworded finding"
		}
		if calls == 3 {
			result = "satisfied"
			evidence = "fresh source-backed evidence"
		}
		findings := types.Findings{
			DecisionReviews: []types.DecisionReview{{DecisionID: decisionID, Result: result, Evidence: evidence}},
			Items:           []types.Finding{}, Summary: result,
		}
		if result != "satisfied" {
			findings.Items = append(findings.Items, types.Finding{DecisionID: decisionID, Severity: "warning", Description: description, Action: types.ActionAskUser})
		}
		raw, err := json.Marshal(findings)
		if err != nil {
			t.Fatal(err)
		}
		return &StepOutcome{NeedsApproval: result != "satisfied", Findings: string(raw)}, nil
	}}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || steps[0].FindingsJSON == nil {
		t.Fatalf("load repeated adverse round: %v", err)
	}
	adverse, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(adverse.Items) != 1 || adverse.Items[0].DecisionID != decisionID {
		t.Fatalf("reworded adverse round accumulated identities: %+v", adverse.Items)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{adverse.Items[0].ID}); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
	if calls != 3 {
		t.Fatalf("review calls = %d, want 3", calls)
	}
}

func TestMergeOutstandingFindingsJSON_EmptyCurrentRoundClearsStaleDecisionReviews(t *testing.T) {
	prior := `{"decision_reviews":[{"decision_id":"decision-1","result":"satisfied","evidence":"stale"}],"findings":[{"id":"review-1","severity":"warning","file":"other.go","description":"ordinary outstanding finding","action":"ask-user"}],"summary":"blocked"}`
	for _, current := range []string{"", `{"findings":[],"summary":"clean"}`, `{"decision_reviews":[],"findings":[],"summary":"clean"}`} {
		merged := mergeOutstandingFindingsJSON(prior, current, nil)
		parsed, err := types.ParseFindingsJSON(merged)
		if err != nil {
			t.Fatal(err)
		}
		if len(parsed.DecisionReviews) != 0 {
			t.Fatalf("current %q retained stale reviews: %+v", current, parsed.DecisionReviews)
		}
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
