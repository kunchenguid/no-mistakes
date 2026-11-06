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

// abortParkedGate aborts a gate the executor has just re-parked. A step status
// read from the database can land before the executor registers the park, and a
// status the step already had before the previous response cannot tell the new
// park from the answered one, so the abort retries until a gate is there.
func abortParkedGate(t *testing.T, exec *Executor, step types.StepName) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(step, types.ActionAbort, nil)
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			t.Fatalf("abort the parked gate: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: coverageBinding(t, database, run.ID)})
		if err != nil && strings.Contains(err.Error(), "no step awaiting approval") && !time.Now().After(deadline) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "outstanding") {
			t.Fatalf("unsettled error/warning did not block coverage: %v", err)
		}
		break
	}
	abortParkedGate(t, exec, types.StepReview)
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
//
// The gate must show the moved instance under its own new id for that refusal
// to be reachable at all, so the fixture takes the route a real run takes: a
// covering rereview verifies the selected finding away - which is what drops it
// from the outstanding set, and with it the retained id the next gate would
// otherwise carry - a second finding keeps the step parked, and only then does
// the reviewer report the original concern again at a new line.
func TestExecutor_DispositionsRefuseAnEarlierFixAtANewLine(t *testing.T) {
	for _, mode := range []string{"ignore", "resolved_external"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			grant := types.Finding{ID: "original", File: "first.go", Line: 10, Severity: types.FindingSeverityError, Description: "grant missing", Action: types.ActionAskUser}
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				switch calls {
				case 1:
					return dispositionCoverageOutcome(t, sctx, []types.Finding{grant}, []string{"first.go"}), nil
				case 2:
					// Covers the selected finding's file and reports nothing
					// in it, which is what verifies it away, and parks on a
					// finding in the other file instead.
					return dispositionCoverageOutcome(t, sctx, []types.Finding{
						{ID: "unrelated", File: "second.go", Line: 40, Severity: types.FindingSeverityWarning, Description: "unrelated concern", Action: types.ActionAskUser},
					}, []string{"first.go"}), nil
				default:
					// Same clearing move for the second selection, and the
					// original concern comes back at a new line under a new id.
					moved := grant
					moved.ID = "reissued"
					moved.Line = 19
					return dispositionCoverageOutcome(t, sctx, []types.Finding{moved}, []string{"second.go"}), nil
				}
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
			waitForParkedGateFinding(t, exec, "unrelated")
			if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"unrelated"}, nil, nil, nil, ""); err != nil {
				t.Fatal(err)
			}
			waitForParkedGateFinding(t, exec, "reissued")

			var err error
			if mode == "ignore" {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, []string{"reissued"}, nil, nil, "")
			} else {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"reissued"}, nil, nil, nil, "grant verified on the machine")
			}
			var refusal *RespondRefusal
			if !errors.As(err, &refusal) || len(refusal.DeclinedEarlierFix) != 1 || refusal.DeclinedEarlierFix[0] != "reissued" {
				t.Fatalf("a moved earlier fix was accepted as settled: %v", err)
			}
		})
	}
}

// waitForParkedGateFinding blocks until the executor has registered a parked
// gate whose payload shows id. A step status read from the database can land
// before the gate is registered, and after the first fix round the status is
// already fix_review, so it cannot tell the new park from the answered one.
func waitForParkedGateFinding(t *testing.T, exec *Executor, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		exec.mu.Lock()
		waiting, gate := exec.waiting, exec.waitingFindings
		exec.mu.Unlock()
		if waiting {
			for _, got := range findingIDsInPayloadOrder(gate) {
				if got == id {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no parked gate showed finding %s; last gate: %s", id, gate)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A gate that shows one fingerprint twice cannot be settled, so the only
// response left is to fix both sites. That selection must still protect the
// fingerprint: an earlier explicit ignore cannot settle a site the operator
// then chose to fix, however many findings shared the ambiguous payload.
func TestFilterSettledReviewFindings_AnAmbiguousGateStillCreditsTheFixSelection(t *testing.T) {
	item := types.Finding{ID: "first", File: "first.go", Line: 10, Severity: types.FindingSeverityWarning, Description: "duplicate check missing", Action: types.ActionAskUser}
	firstRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	empty := db.DeclinedSelectionJSON
	ignoreSource := db.RoundSelectionSourceUserIgnored

	twinA := item
	twinA.ID = "twin-a"
	twinB := item
	twinB.ID = "twin-b"
	twinB.Line = 50
	secondRound, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{twinA, twinB}})
	chosen := `["twin-a","twin-b"]`
	fixSource := db.RoundSelectionSourceUser

	remaining := item
	remaining.ID = "still-open"
	remaining.Line = 70
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{remaining}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}})

	rounds := []*db.StepRound{
		{FindingsJSON: &firstRound, SelectionSource: &ignoreSource, SelectedFindingIDs: &empty},
		{FindingsJSON: &secondRound, SelectionSource: &fixSource, SelectedFindingIDs: &chosen},
	}
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 1 {
		t.Fatalf("an ambiguous fix round lost its selection and the finding was settled: %+v %v", parsed.Items, err)
	}
	if got := SettledReviewFindingCount(rounds); got != 0 {
		t.Fatalf("settled count = %d, want 0: the operator chose to fix that finding", got)
	}
}

// A decline taken on one of two findings that differ only in line settles that
// instance and nothing else, and it stops being matched by content once the
// reviewer moves it, so the operator is asked again rather than having a moved
// finding settled for them.
func TestFilterSettledReviewFindings_TwinFindingsSettleOneInstance(t *testing.T) {
	twin := types.Finding{File: "first.go", Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	first, second := twin, twin
	first.ID, first.Line = "twin-a", 10
	second.ID, second.Line = "twin-b", 40
	shown, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{first, second}})
	chosen := `["twin-a"]`
	source := db.RoundSelectionSourceUser
	rounds := []*db.StepRound{{FindingsJSON: &shown, SelectionSource: &source, SelectedFindingIDs: &chosen}}

	scope := &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}
	for _, tc := range []struct {
		name      string
		line      int
		wantItems int
	}{
		{"the declined instance is settled", 40, 1},
		{"a moved instance is reported again", 44, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			again, later := twin, twin
			again.ID, again.Line = "twin-a-again", 10
			later.ID, later.Line = "twin-b-again", tc.line
			raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{again, later}, ReviewCoverageScope: scope})
			filtered, err := filterSettledReviewFindings(raw, rounds)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(filtered)
			if err != nil || len(parsed.Items) != tc.wantItems {
				t.Fatalf("findings = %+v %v, want %d", parsed.Items, err, tc.wantItems)
			}
			if tc.wantItems == 1 && parsed.Items[0].ID != "twin-a-again" {
				t.Fatalf("the wrong instance was settled: %+v", parsed.Items)
			}
		})
	}
	if got := SettledReviewFindingCount(rounds); got != 1 {
		t.Fatalf("settled count = %d, want 1: one instance was declined", got)
	}
}

// The PR body reports how many findings the operator settled without a fix.
// One finding settled, moved by a later round, and settled again is one such
// decision, so the published count follows the findings, not the instances.
func TestSettledReviewFindingCount_AResettledMovedFindingCountsOnce(t *testing.T) {
	twin := types.Finding{File: "first.go", Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	first, second := twin, twin
	first.ID, first.Line = "twin-a", 10
	second.ID, second.Line = "twin-b", 40
	pair, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{first, second}})

	survivor := twin
	survivor.ID, survivor.Line = "survivor", 44
	moved, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{survivor}})

	empty := db.DeclinedSelectionJSON
	source := db.RoundSelectionSourceUserIgnored
	rounds := []*db.StepRound{
		{FindingsJSON: &pair, SelectionSource: &source, SelectedFindingIDs: &empty},
	}
	if got := SettledReviewFindingCount(rounds); got != 2 {
		t.Fatalf("settled count = %d, want 2: the operator declined two findings", got)
	}
	rounds = append(rounds, &db.StepRound{FindingsJSON: &moved, SelectionSource: &source, SelectedFindingIDs: &empty})
	if got := SettledReviewFindingCount(rounds); got != 2 {
		t.Fatalf("settled count = %d, want 2: declining the moved survivor again settles no new finding", got)
	}
}

// axi renders the reviewer's summary beside the gate's findings, so the filter
// that stopped reporting one of them cannot leave a sentence counting it.
func TestFilterSettledReviewFindings_ASuppressedGateDropsTheStaleSummary(t *testing.T) {
	settledItem := types.Finding{ID: "grant", File: "first.go", Line: 3, Severity: types.FindingSeverityWarning, Description: "private grant not visible", Action: types.ActionAskUser}
	live := types.Finding{ID: "bug", File: "second.go", Line: 9, Severity: types.FindingSeverityError, Description: "off-by-one", Action: types.ActionAskUser}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{settledItem, live}})
	chosen := `["bug"]`
	source := db.RoundSelectionSourceUser
	rounds := []*db.StepRound{{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &chosen}}

	raw, _ := types.MarshalFindingsJSON(types.Findings{
		Items:               []types.Finding{settledItem, live},
		Summary:             "2 findings",
		ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}},
	})
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != "bug" {
		t.Fatalf("findings = %+v %v, want only the unsettled one", parsed.Items, err)
	}
	if parsed.Summary != "" {
		t.Fatalf("summary = %q, want none: it counted a finding the gate no longer lists", parsed.Summary)
	}
}

// The filter withholds a disposition for a finding a human round chose to fix,
// and two such rounds about one content protect it at least as well as one.
// Otherwise an ignore-only round's complement settles work the operator asked
// for and every later round stops reporting it.
func TestFilterSettledReviewFindings_AContentFixedTwiceIsNeverSettled(t *testing.T) {
	item := types.Finding{File: "first.go", Severity: types.FindingSeverityWarning, Description: "missing nil check", Action: types.ActionAskUser}
	at := func(id string, line int) string {
		shown := item
		shown.ID, shown.Line = id, line
		encoded, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{shown}})
		return encoded
	}
	first, second, declined := at("first", 10), at("second", 19), at("third", 27)
	firstChoice, secondChoice := `["first"]`, `["second"]`
	empty := db.DeclinedSelectionJSON
	fixSource := db.RoundSelectionSourceUser
	ignoreSource := db.RoundSelectionSourceUserIgnored
	rounds := []*db.StepRound{
		{FindingsJSON: &first, SelectionSource: &fixSource, SelectedFindingIDs: &firstChoice},
		{FindingsJSON: &second, SelectionSource: &fixSource, SelectedFindingIDs: &secondChoice},
		{FindingsJSON: &declined, SelectionSource: &ignoreSource, SelectedFindingIDs: &empty},
	}

	later := item
	later.ID, later.Line = "fourth", 31
	raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{later}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}})

	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 1 {
		t.Fatalf("a content the operator fixed twice was settled by a later complement: %+v %v", parsed.Items, err)
	}
	if got := SettledReviewFindingCount(rounds); got != 0 {
		t.Fatalf("settled count = %d, want 0: the operator chose to fix that content", got)
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

// Two findings that differ only in line are decided one instance at a time, so
// both the mixed response and the decline-everything response are accepted. A
// gate that shows the same finding twice as an identical record cannot be
// decided apart at all, so a disposition naming either one is refused.
func TestExecutor_DispositionsSettleTwinFindingsOneInstanceAtATime(t *testing.T) {
	twin := types.Finding{File: "first.go", Severity: types.FindingSeverityWarning, Description: "grant missing", Action: types.ActionAskUser}
	for _, tc := range []struct {
		name        string
		secondLine  int
		selected    []string
		ignored     []string
		wantRefusal string
	}{
		{name: "mixed fix and decline", secondLine: 40, selected: []string{"twin-a"}, ignored: []string{"twin-b"}},
		{name: "decline both", secondLine: 40, ignored: []string{"twin-a", "twin-b"}},
		{name: "identical records", secondLine: 10, selected: []string{"twin-a"}, ignored: []string{"twin-b"}, wantRefusal: "twin-b cannot be declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			secondLine := tc.secondLine
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				first, second := twin, twin
				first.ID, first.Line = "twin-a", 10
				second.ID, second.Line = "twin-b", secondLine
				return dispositionCoverageOutcome(t, sctx, []types.Finding{first, second}, []string{"first.go"}), nil
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
			dispositions, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, tc.selected, tc.ignored, nil, nil, "")
			if tc.wantRefusal != "" {
				var refusal *RespondRefusal
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, tc.wantRefusal) {
					t.Fatalf("a decision on an identical record was accepted: %v", err)
				}
				if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatalf("declining a twin finding was refused: %v", err)
			}
			if strings.Join(dispositions.Ignored, ",") != strings.Join(tc.ignored, ",") {
				t.Fatalf("ignored = %v, want %v", dispositions.Ignored, tc.ignored)
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
			abortParkedGate(t, exec, types.StepReview)
		})
	}
}

// A daemon restart between recording an external settlement and running the
// review round it starts must not strand the run: the decision and its reason
// are already durable, so recovery replays the settlement instead of parking a
// gate whose every continuation is refused, and the round it starts is an
// ordinary gate the operator can fix.
func TestExecutor_RecoveredExternalRoundContinuesTheReview(t *testing.T) {
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

	continued := `{"findings":[{"id":"continued","severity":"warning","file":"y.js","line":3,` +
		`"description":"the rereview found this instead","action":"ask-user"}]}`
	started := make(chan string, 4)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if sctx.Fixing {
			started <- "fix"
			return &StepOutcome{NeedsApproval: true, Findings: continued, FixSummary: "applied"}, nil
		}
		started <- "review"
		return &StepOutcome{NeedsApproval: true, Findings: continued}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, run, repo, t.TempDir()) }()

	select {
	case trigger := <-started:
		if trigger != "review" {
			t.Fatalf("recovered round ran as %q, want the replayed review", trigger)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery never continued the recorded external settlement")
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"continued"}, nil, nil, nil, "")
		if err == nil || !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the round after a recovered settlement refused a fix response: %v", err)
	}
	select {
	case trigger := <-started:
		if trigger != "fix" {
			t.Fatalf("the accepted fix response ran as %q", trigger)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the accepted fix response never reached the step")
	}

	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil || len(rounds) < 2 {
		t.Fatalf("rounds: %v %d, want the settlement round and the round it started", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceExternal {
		t.Fatalf("selection_source = %v, want the recorded %q", rounds[0].SelectionSource, db.RoundSelectionSourceExternal)
	}
	if rounds[0].UserFindingsJSON == nil || !strings.Contains(*rounds[0].UserFindingsJSON, "grant verified on the machine") {
		t.Fatalf("the operator's reason was erased: %v", rounds[0].UserFindingsJSON)
	}
	if rounds[1].Trigger != "external" {
		t.Fatalf("round 2 trigger = %q, want %q", rounds[1].Trigger, "external")
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	abortParkedGate(t, exec, types.StepReview)
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
