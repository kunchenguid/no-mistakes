package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func dispositionCoverageOutcome(t *testing.T, sctx *StepContext, items []types.Finding, reviewed []string) *StepOutcome {
	t.Helper()
	var findings types.Findings
	if err := json.Unmarshal([]byte(coverageFixtureFindings(t, sctx.Run, sctx.StepResultID, reviewed, []string{"first.go", "second.go"})), &findings); err != nil {
		t.Fatal(err)
	}
	findings.Items = items
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	return &StepOutcome{NeedsApproval: len(items) != 0 || len(reviewed) != 2, Findings: encoded, AutoFixable: len(items) != 0, ReviewedPaths: reviewed, ReviewablePaths: []string{"first.go", "second.go"}, ReviewApprovedHeadSHA: sctx.Run.HeadSHA}
}

func TestExecutor_SettledFindingDoesNotBlockCoverageContinuation(t *testing.T) {
	for _, mode := range []string{"ignore", "resolved_external"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if calls == 4 {
					if sctx.Fixing {
						t.Error("coverage continuation ran a fix")
					}
					return dispositionCoverageOutcome(t, sctx, nil, []string{"first.go", "second.go"}), nil
				}
				id := "original"
				line := 12
				if calls == 2 {
					id = "reissued"
					line = 19
				} else if calls == 3 {
					id = "coverage-reissued"
					line = 27
				}
				return dispositionCoverageOutcome(t, sctx, []types.Finding{{ID: id, File: "first.go", Line: line, Severity: types.FindingSeverityInfo, Description: "private maintainer grant already settled", Action: types.ActionNoOp}}, []string{"first.go"}), nil
			}}
			exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			done := make(chan error, 1)
			go func() { defer close(finished); done <- exec.Execute(ctx, run, repo, workDir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("executor did not exit")
				}
			})
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			var err error
			if mode == "ignore" {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, []string{"original"}, nil, nil, "")
			} else {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"original"}, nil, nil, nil, "private grant verified on the machine")
			}
			if err != nil {
				t.Fatal(err)
			}
			status := types.StepStatusAwaitingApproval
			if mode == "ignore" {
				status = types.StepStatusFixReview
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, status)
			deadline := time.Now().Add(5 * time.Second)
			for attempt := 0; attempt < 2; attempt++ {
				for {
					binding := coverageBinding(t, database, run.ID)
					_, err = exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding})
					if err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("settled finding blocked coverage round %d: %v", attempt+1, err)
					}
					time.Sleep(5 * time.Millisecond)
				}
				if attempt == 0 {
					waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
				}
			}
			deadline = time.Now().Add(5 * time.Second)
			for {
				err = exec.Respond(types.StepReview, types.ActionApprove, nil)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("a Review that settled a finding never parked for a verdict: %v", err)
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("coverage did not finish")
			}
			if calls != 4 {
				t.Fatalf("review turns = %d, want 4", calls)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || steps[0].Status != types.StepStatusCompleted {
				t.Fatalf("review did not complete: %+v %v", steps, err)
			}
			if mode == "resolved_external" {
				rounds, err := database.GetRoundsByStep(steps[0].ID)
				if err != nil || len(rounds) < 2 || rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceExternal || rounds[0].UserFindingsJSON == nil {
					t.Fatalf("external decision was not durable: %+v %v", rounds, err)
				}
				resolved, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
				if err != nil || len(resolved.Items) != 1 || resolved.Items[0].UserInstructions != "private grant verified on the machine" {
					t.Fatalf("external reason was lost: %+v %v", resolved, err)
				}
				if rounds[1].Trigger != "external" {
					t.Fatalf("second round trigger = %q, want external", rounds[1].Trigger)
				}
			}
		})
	}
}

func TestExecutor_ExternalDispositionRequiresReasonAndKeepsOtherFindingsBlocking(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		return dispositionCoverageOutcome(t, sctx, []types.Finding{
			{ID: "settled", File: "first.go", Severity: types.FindingSeverityInfo, Description: "private grant", Action: types.ActionNoOp},
			{ID: "error", File: "second.go", Severity: types.FindingSeverityError, Description: "live defect", Action: types.ActionNoOp},
			{ID: "warning", File: "second.go", Severity: types.FindingSeverityWarning, Description: "live concern", Action: types.ActionAskUser},
		}, []string{"first.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); _ = exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() { cancel(); <-finished })
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"settled"}, nil, nil, nil, " "); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("missing reason was accepted: %v", err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"settled"}, nil, nil, nil, "verified outside review worktree"); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: coverageBinding(t, database, run.ID)}); err == nil || !strings.Contains(err.Error(), "outstanding") {
		t.Fatalf("unsettled error/warning did not block coverage: %v", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFilterSettledReviewFindings_AnExplicitIgnoreIsStickyButAnApprovalIsNot(t *testing.T) {
	item := types.Finding{ID: "original", File: "first.go", Severity: types.FindingSeverityInfo, Description: "grant settled", Action: types.ActionNoOp}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	item.ID = "new-id"
	item.Line = 31
	current := types.Findings{Items: []types.Finding{item}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}}
	raw, _ := types.MarshalFindingsJSON(current)
	selection := db.DeclinedSelectionJSON
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"approve", db.RoundSelectionSourceUserDeclined, 1},
		{"explicit ignore", db.RoundSelectionSourceUserIgnored, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			rounds := []*db.StepRound{{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &selection}}
			filtered, err := filterSettledReviewFindings(raw, rounds)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(filtered)
			if err != nil || len(parsed.Items) != tc.want {
				t.Fatalf("items = %d, want %d: %v", len(parsed.Items), tc.want, err)
			}
		})
	}
}

// A reviewer's open question is resolved by its answer, and the unreadable
// history marker is pipeline-owned, so no finding disposition may suppress
// either one. Both would otherwise let Review certify a head with a question
// still open in the conversation.
func TestFilterSettledReviewFindings_AQuestionAndAReservedMarkerAreNeverSettled(t *testing.T) {
	question := types.Finding{ID: "question-q1", Category: types.FindingCategoryReviewQuestion, Severity: types.FindingSeverityWarning, Description: "which grant applies?", Action: types.ActionAskUser}
	marker := types.Finding{ID: ReviewQuestionsUnreadableFindingID, Severity: types.FindingSeverityWarning, Description: "question history incomplete", Action: types.ActionAskUser}
	code := types.Finding{ID: "code", File: "first.go", Severity: types.FindingSeverityInfo, Description: "grant settled", Action: types.ActionNoOp}
	shown := []types.Finding{question, marker, code}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: shown})
	settled, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{
		{ID: "question-q1", Category: types.FindingCategoryReviewQuestion, Severity: types.FindingSeverityWarning, Description: "which grant applies?", Action: types.ActionAskUser, UserInstructions: "settled on the machine"},
		{ID: ReviewQuestionsUnreadableFindingID, Severity: types.FindingSeverityWarning, Description: "question history incomplete", Action: types.ActionAskUser, UserInstructions: "settled on the machine"},
		{ID: "code", File: "first.go", Severity: types.FindingSeverityInfo, Description: "grant settled", Action: types.ActionNoOp, UserInstructions: "settled on the machine"},
	}})
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: shown, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}})
	selection := db.DeclinedSelectionJSON
	for _, tc := range []struct{ name, source string }{
		{"ignore", db.RoundSelectionSourceUserIgnored},
		{"resolved external", db.RoundSelectionSourceExternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			round := &db.StepRound{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &selection}
			if source == db.RoundSelectionSourceExternal {
				round.UserFindingsJSON = &settled
			}
			filtered, err := filterSettledReviewFindings(raw, []*db.StepRound{round})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(filtered)
			if err != nil {
				t.Fatal(err)
			}
			kept := map[string]bool{}
			for _, item := range parsed.Items {
				kept[item.ID] = true
			}
			if !kept["question-q1"] || !kept[ReviewQuestionsUnreadableFindingID] || kept["code"] {
				t.Fatalf("settled set = %+v, want the question and the marker kept and the code finding dropped", parsed.Items)
			}
		})
	}
}

// A finding an earlier round chose to fix is never settled, at its own line or
// at a new one: the gate refuses the disposition, so a recorded one can only
// come from a stale database, and honouring it would carry the operator's fix
// selection out of the gate and out of the outstanding set.
func TestFilterSettledReviewFindings_AnEarlierFixSurvivesALaterDispositionAtANewLine(t *testing.T) {
	item := types.Finding{ID: "original", File: "first.go", Line: 10, Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	firstRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	chosen := `["original"]`
	fixSource := db.RoundSelectionSourceUser

	reissued := item
	reissued.ID = "reissued"
	reissued.Line = 19
	secondRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{reissued}})
	settledItem := reissued
	settledItem.UserInstructions = "grant verified on the machine"
	settled, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{settledItem}})
	empty := db.DeclinedSelectionJSON
	externalSource := db.RoundSelectionSourceExternal

	later := item
	later.ID = "third"
	later.Line = 27
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{later}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}})

	rounds := []*db.StepRound{
		{FindingsJSON: &firstRound, SelectionSource: &fixSource, SelectedFindingIDs: &chosen},
		{FindingsJSON: &secondRound, SelectionSource: &externalSource, SelectedFindingIDs: &empty, UserFindingsJSON: &settled},
	}
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 1 {
		t.Fatalf("a moved line carried an earlier fix selection out of the gate: %+v %v", parsed.Items, err)
	}
	if got := SettledReviewFindingCount(rounds); got != 0 {
		t.Fatalf("settled count = %d, want 0: nothing the filter removed was settled", got)
	}
}

// A fix response may omit a finding an earlier round already decided, and the
// gate accepts that omission without the operator ruling on it. Crediting it as
// a decline would suppress a finding the same operator asked to have fixed.
func TestFilterSettledReviewFindings_AnOmissionIsNeverADecline(t *testing.T) {
	grant := types.Finding{ID: "grant", File: "first.go", Line: 10, Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	note := types.Finding{ID: "note", File: "second.go", Line: 5, Severity: types.FindingSeverityInfo, Description: "stale note", Action: types.ActionNoOp}
	firstRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{grant, note}})
	firstChoice := `["grant","note"]`
	fixSource := db.RoundSelectionSourceUser

	fresh := types.Finding{ID: "fresh", File: "second.go", Line: 41, Severity: types.FindingSeverityError, Description: "new call is unchecked", Action: types.ActionAskUser}
	secondRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{grant, fresh}})
	secondChoice := `["fresh"]`

	moved := grant
	moved.ID = "grant-again"
	moved.Line = 12
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{moved}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}})

	rounds := []*db.StepRound{
		{FindingsJSON: &firstRound, SelectionSource: &fixSource, SelectedFindingIDs: &firstChoice},
		{FindingsJSON: &secondRound, SelectionSource: &fixSource, SelectedFindingIDs: &secondChoice},
	}
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 1 {
		t.Fatalf("an omission settled a finding the operator chose to fix: %+v %v", parsed.Items, err)
	}
	if got := SettledReviewFindingCount(rounds); got != 0 {
		t.Fatalf("settled count = %d, want 0: no finding was settled without a fix", got)
	}
}

// A re-minted finding at a new line is the same finding, so a response that
// declines or externally settles what an earlier round chose to fix is refused
// there too. Accepting it would echo a decision the filter never applies.
func TestExecutor_DispositionsRefuseAnEarlierFixAtANewLine(t *testing.T) {
	for _, mode := range []string{"ignore", "resolved_external"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				id, line := "original", 10
				if calls > 1 {
					id, line = "reissued", 19
				}
				return dispositionCoverageOutcome(t, sctx, []types.Finding{
					{ID: id, File: "first.go", Line: line, Severity: types.FindingSeverityError, Description: "grant missing", Action: types.ActionAskUser},
				}, []string{"first.go"}), nil
			}}
			exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			go func() { defer close(finished); _ = exec.Execute(ctx, run, repo, workDir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("executor did not exit")
				}
			})

			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"original"}, nil, nil, nil, ""); err != nil {
				t.Fatal(err)
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

			var err error
			deadline := time.Now().Add(5 * time.Second)
			for {
				if mode == "ignore" {
					_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, []string{"reissued"}, nil, nil, "")
				} else {
					_, err = exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"reissued"}, nil, nil, nil, "grant verified on the machine")
				}
				if err == nil || !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			var refusal *RespondRefusal
			if !errors.As(err, &refusal) || len(refusal.DeclinedEarlierFix) != 1 || refusal.DeclinedEarlierFix[0] != "reissued" {
				t.Fatalf("a moved earlier fix was accepted as settled: %v", err)
			}
		})
	}
}

func TestFilterSettledReviewFindings_AmbiguousFingerprintFailsClosed(t *testing.T) {
	item := types.Finding{ID: "first", File: "first.go", Severity: types.FindingSeverityInfo, Description: "same", Action: types.ActionNoOp}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	selection := db.DeclinedSelectionJSON
	source := db.RoundSelectionSourceUserIgnored
	rounds := []*db.StepRound{{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &selection}}
	current := types.Findings{Items: []types.Finding{item, item}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}}
	raw, _ := types.MarshalFindingsJSON(current)
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 2 {
		t.Fatalf("ambiguous findings were dropped: %+v %v", parsed, err)
	}
}

// An open question is settled by its answer and a pipeline-owned finding is
// re-derived every turn, so neither can be named in an external disposition.
// A finding an earlier round chose to fix cannot be settled externally either,
// for the same reason --ignore refuses it: a gate response never reverses an
// applied fix, and accepting one here would record a decision the filter can
// never apply.
func TestExecutor_ExternalDispositionRefusesWhatItCannotSettle(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		return dispositionCoverageOutcome(t, sctx, []types.Finding{
			{ID: "question-q1", Category: types.FindingCategoryReviewQuestion, Severity: types.FindingSeverityWarning, Description: "which grant applies?", Action: types.ActionAskUser},
			{ID: ReviewQuestionsUnreadableFindingID, Severity: types.FindingSeverityWarning, Description: "question history incomplete", Action: types.ActionAskUser},
			{ID: "code", File: "first.go", Line: 10, Severity: types.FindingSeverityError, Description: "grant missing", Action: types.ActionAskUser},
		}, []string{"first.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); _ = exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() { cancel(); <-finished })

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	for _, id := range []string{"question-q1", ReviewQuestionsUnreadableFindingID} {
		_, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{id}, nil, nil, nil, "settled on the machine")
		if err == nil || !strings.Contains(err.Error(), "cannot settle "+id) {
			t.Fatalf("%s was accepted as externally settled: %v", id, err)
		}
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"code"}, []string{"question-q1", ReviewQuestionsUnreadableFindingID}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"code"}, nil, nil, nil, "settled on the machine")
		if err == nil || !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) || len(refusal.DeclinedEarlierFix) != 1 || refusal.DeclinedEarlierFix[0] != "code" {
		t.Fatalf("external disposition was accepted over an applied fix: %v", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
}

// A disposition is matched on a later round by fingerprint, so one the gate
// shows twice under identical content could never be matched again. Recording
// it would echo a decision the run can never act on.
func TestExecutor_DispositionsRefuseAnAmbiguousFingerprint(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	twin := types.Finding{File: "first.go", Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		first, second := twin, twin
		first.ID, first.Line = "twin-a", 10
		second.ID, second.Line = "twin-b", 40
		return dispositionCoverageOutcome(t, sctx, []types.Finding{first, second}, []string{"first.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); _ = exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() { cancel(); <-finished })

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	_, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"twin-a"}, nil, nil, nil, "grant verified on the machine")
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "twin-a cannot be settled") {
		t.Fatalf("external disposition on an ambiguous finding was accepted: %v", err)
	}
	_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"twin-a"}, []string{"twin-b"}, nil, nil, "")
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "twin-b cannot be declined") {
		t.Fatalf("ignore of an ambiguous finding was accepted: %v", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
}

// Recovery re-parks a round whose external disposition is already recorded.
// A fix response there would overwrite the only column holding the operator's
// reason, so it is refused and the gate stays parked.
func TestExecutor_RecoveredExternalRoundRefusesAFixResponse(t *testing.T) {
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
	findings := declinedTestFindings
	round, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	empty := db.DeclinedSelectionJSON
	settled := `{"findings":[{"id":"journal-version-deduplication","severity":"error","file":"x.js","line":497,` +
		`"description":"dedup checks only the last event; check all prior events instead","action":"ask-user",` +
		`"user_instructions":"grant verified on the machine"}]}`
	if err := database.SetStepRoundUserDecision(round.ID, &empty, db.RoundSelectionSourceExternal, &settled, types.ResponseOrigin("interactive")); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(stepResult.ID, declinedTestFindings); err != nil {
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

	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, declinedTestFindings)}, nil)
	events := collectEvents(exec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, run, repo, t.TempDir()) }()
	waitForStepEvent(t, events, ipc.EventStepCompleted, types.StepReview)

	for _, tc := range []struct {
		name     string
		selected []string
		ignore   []string
	}{
		{"ignore only", nil, []string{"journal-version-deduplication"}},
		{"with a selection", []string{"journal-version-deduplication"}, nil},
	} {
		deadline := time.Now().Add(5 * time.Second)
		var err error
		for {
			_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, tc.selected, tc.ignore, nil, nil, "")
			if err == nil || !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		var refusal *RespondRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "already records an external finding disposition") {
			t.Fatalf("%s: fix response overwrote a recorded external disposition: %v", tc.name, err)
		}
	}

	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceExternal {
		t.Fatalf("selection_source = %v, want the recorded %q", rounds[0].SelectionSource, db.RoundSelectionSourceExternal)
	}
	if rounds[0].UserFindingsJSON == nil || !strings.Contains(*rounds[0].UserFindingsJSON, "grant verified on the machine") {
		t.Fatalf("the operator's reason was erased: %v", rounds[0].UserFindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}
}

// A settled finding leaves the report, so the findings themselves can no
// longer hold the gate: an `info` ask-user finding parks only while it is
// still reported, and the filter is what stops reporting it. The settlement
// itself must park, or Review certifies the head with no operator verdict.
func TestExecutor_ASettledNonBlockingFindingStillParksForAVerdict(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		id, line := "note", 10
		if calls > 1 {
			id, line = "note-reissued", 24
		}
		outcome := dispositionCoverageOutcome(t, sctx, []types.Finding{
			{ID: id, File: "first.go", Line: line, Severity: types.FindingSeverityInfo, Description: "private maintainer grant already settled", Action: types.ActionAskUser},
		}, []string{"first.go", "second.go"})
		// The real review step parks a non-blocking finding only through
		// hasAskUserFindingsJSON; it never asks for approval itself.
		outcome.NeedsApproval = false
		outcome.AutoFixable = false
		return outcome, nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() { defer close(finished); done <- exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("executor did not exit")
		}
	})

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"note"}, nil, nil, nil, "grant verified on the machine"); err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("steps: %v %+v", err, steps)
	}
	stepID := steps[0].ID
	deadline := time.Now().Add(5 * time.Second)
	for {
		rounds, err := database.GetRoundsByStep(stepID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current[0].Status == types.StepStatusCompleted {
			t.Fatal("Review completed over a settled finding with no operator verdict")
		}
		if len(rounds) == 2 && current[0].Status == types.StepStatusAwaitingApproval {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the settled Review never parked for a verdict: rounds = %d, status = %s", len(rounds), current[0].Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approved Review did not finish")
	}
	if calls != 2 {
		t.Fatalf("review turns = %d, want 2", calls)
	}
	steps, err = database.GetStepsByRun(run.ID)
	if err != nil || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("approved review did not complete: %+v %v", steps, err)
	}
}
