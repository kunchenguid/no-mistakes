package pipeline

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type findingWithAction struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

func mustParseFindingsWithAction(t *testing.T, raw string) []findingWithAction {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings JSON: %v", err)
	}
	out := make([]findingWithAction, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		out = append(out, findingWithAction{ID: item.ID, Action: item.Action})
	}
	return out
}

// TestExecutor_UnresolvedAskUserFindingsSurviveAnEmptyReReviewRound is the
// DEFECT 1 regression: round 1 reports 5 findings, 3 of them ask-user. The
// operator fixes only the other 2 (`axi respond --action fix --findings
// review-2,review-5`), explicitly leaving the 3 ask-user findings alone.
// Round 2's re-review is scoped to the fix diff only and reports nothing new.
// The step must NOT complete: the 3 unresolved ask-user findings were never
// named in a respond action and must keep blocking.
func TestExecutor_UnresolvedAskUserFindingsSurviveAnEmptyReReviewRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"copy is dishonest","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"auto-fix"},
		{"id":"review-3","severity":"error","description":"another honesty issue","action":"ask-user"},
		{"id":"review-4","severity":"error","description":"a third honesty issue","action":"ask-user"},
		{"id":"review-5","severity":"warning","description":"lint nit","action":"auto-fix"}
	],"summary":"5 findings"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			// Round 2: a re-review scoped to only the fix diff. It legitimately
			// finds nothing new in that diff.
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2", "review-5"}); err != nil {
		t.Fatal(err)
	}

	// On the pre-fix executor, round 2's empty findings complete the step
	// immediately, silently dropping review-1/3/4. It must instead re-park
	// with those three ask-user findings still open.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].Status != types.StepStatusFixReview {
		t.Fatalf("step status = %s, want fix_review: unresolved ask-user findings must still block", dbSteps[0].Status)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected the step's stored findings to still carry the unresolved ask-user items")
	}
	items := mustParseFindingsWithAction(t, *dbSteps[0].FindingsJSON)
	byID := map[string]string{}
	for _, it := range items {
		byID[it.ID] = it.Action
	}
	for _, want := range []string{"review-1", "review-3", "review-4"} {
		if byID[want] != "ask-user" {
			t.Errorf("expected carried-forward ask-user finding %s to survive the empty re-review round, got action %q (present=%v)", want, byID[want], byID[want] != "")
		}
	}
	if _, stillThere := byID["review-2"]; stillThere {
		t.Errorf("review-2 was selected and fixed; it must not still be reported as outstanding: %v", byID)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_EmptyRoundDoesNotRepublishAStaleRiskRationale covers the more
// common shape of "this round found nothing": the step returns no findings
// payload at all, so the merge hands the carried set straight back. The
// carried rationale was written about the larger set, and this is exactly
// what the gate shows and what the PR body publishes on approve, so it must
// not read as the current assessment.
func TestExecutor_EmptyRoundDoesNotRepublishAStaleRiskRationale(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	const staleRationale = "three unresolved blockers require human approval"
	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"still open","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"auto-fix"},
		{"id":"review-3","severity":"warning","description":"lint nit","action":"auto-fix"}
	],"summary":"3 findings","risk_level":"high","risk_rationale":"` + staleRationale + `"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
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
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2", "review-3"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("expected only the unresolved finding to remain, got %d", len(parsed.Items))
	}
	if parsed.RiskRationale == staleRationale {
		t.Errorf("the gate republishes %q as the current assessment over %d outstanding finding", parsed.RiskRationale, len(parsed.Items))
	}
	if !strings.Contains(parsed.RiskRationale, staleRationale) {
		t.Errorf("the earlier assessment is the most substantive text the operator has; it should be marked, not discarded: %q", parsed.RiskRationale)
	}
	if !strings.Contains(parsed.RiskRationale, "earlier round") {
		t.Errorf("risk_rationale = %q, want it marked as describing an earlier, larger finding set", parsed.RiskRationale)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_CarriedForwardFindingsSurviveIDCollisionWithFreshRoundOutput
// covers a hazard the carry-forward fix itself could introduce: a fresh
// round's own findings are normalized against only that round's own item
// count, so a genuinely new, unrelated finding can land on the same
// positional ID ("review-1") as a DIFFERENT finding carried forward from an
// earlier round. Both must survive as distinct findings, not silently
// collapse into one ID that a later `axi respond --findings review-1` would
// ambiguously apply to.
func TestExecutor_CarriedForwardFindingsSurviveIDCollisionWithFreshRoundOutput(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"old carried-forward issue","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"auto-fix"}
	],"summary":"2 findings"}`

	// Round 2's own output has no explicit id, so normalizeFindingsJSON
	// assigns it "review-1" (item index 1 of THIS round's own list) - the
	// same string already used by the carried-forward finding above.
	round2Findings := `{"findings":[{"severity":"error","description":"new unrelated issue","action":"ask-user"}],"summary":"1 finding"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			return &StepOutcome{Findings: round2Findings}, nil
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
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("expected both the carried and the new finding to survive as distinct items, got %d: %#v", len(parsed.Items), parsed.Items)
	}
	ids := map[string]string{}
	for _, item := range parsed.Items {
		if existing, ok := ids[item.ID]; ok {
			t.Fatalf("two distinct findings share id %q: %q and %q", item.ID, existing, item.Description)
		}
		ids[item.ID] = item.Description
	}
	var sawOld, sawNew bool
	for _, desc := range ids {
		if desc == "old carried-forward issue" {
			sawOld = true
		}
		if desc == "new unrelated issue" {
			sawNew = true
		}
	}
	if !sawOld || !sawNew {
		t.Fatalf("expected both findings present with distinct ids, got %#v", ids)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_UnresolvedFindingsSurviveAFixResponseThatSelectsNoFindings
// covers the other fully supported shape of a fix response: one that names no
// findings at all (`axi respond --action fix` with only an added finding, the
// TUI's user-added-finding path, or Respond(step, ActionFix, nil)). Nothing
// was resolved, so nothing may leave the carry set - otherwise the next round
// reporting nothing new completes the step with the ask-user finding dropped,
// which is exactly the defect this commit exists to close.
func TestExecutor_UnresolvedFindingsSurviveAFixResponseThatSelectsNoFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"copy is dishonest","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"ask-user"}
	],"summary":"2 findings"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
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

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected the step's stored findings to still carry the unresolved ask-user items")
	}
	items := mustParseFindingsWithAction(t, *dbSteps[0].FindingsJSON)
	byID := map[string]string{}
	for _, it := range items {
		byID[it.ID] = it.Action
	}
	for _, want := range []string{"review-1", "review-2"} {
		if byID[want] != "ask-user" {
			t.Errorf("a fix response that selected no findings resolved nothing, so %s must still be outstanding; got action %q", want, byID[want])
		}
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_CarriedFindingsDoNotRepublishAStaleSummaryCount pins the
// operator-facing honesty of the gate: when a round reports nothing of its
// own, the carried payload becomes the step's presented assessment verbatim.
// Its summary must count the findings that actually remain, not the larger
// set an earlier round summarized before some of them were resolved.
func TestExecutor_CarriedFindingsDoNotRepublishAStaleSummaryCount(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"copy is dishonest","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"auto-fix"},
		{"id":"review-3","severity":"error","description":"another honesty issue","action":"ask-user"},
		{"id":"review-4","severity":"error","description":"a third honesty issue","action":"ask-user"},
		{"id":"review-5","severity":"warning","description":"lint nit","action":"auto-fix"}
	],"summary":"5 findings"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
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
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2", "review-5"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(parsed.Items) != 3 {
		t.Fatalf("expected the 3 unresolved findings to remain, got %d", len(parsed.Items))
	}
	count, ok := leadingCount(parsed.Summary)
	if !ok {
		t.Fatalf("summary %q carries no item count to check", parsed.Summary)
	}
	if count != len(parsed.Items) {
		t.Errorf("summary %q claims %d findings but %d remain outstanding", parsed.Summary, count, len(parsed.Items))
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// leadingCount reads the leading integer of a findings summary (e.g. "3
// selected findings" -> 3). Reports false when the summary does not start
// with a number.
func leadingCount(summary string) (int, bool) {
	end := 0
	for end < len(summary) && summary[end] >= '0' && summary[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(summary[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// TestExecutor_StepWithCompleteRoundsDoesNotCarryStaleFindings pins the other
// half of the scoping rule. A step that does not declare its rounds can be
// scope limited (CI, for example, re-polls the live check rollup every round)
// is asserting that a fresh round is a complete current assessment. Carrying
// there would re-park the operator on state the pipeline can currently
// disprove - "CI check failing" while the forge reports it green.
func TestExecutor_StepWithCompleteRoundsDoesNotCarryStaleFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"ci-1","severity":"error","description":"CI check failing: build","action":"ask-user"},
		{"id":"ci-2","severity":"error","description":"CI check failing: lint","action":"ask-user"}
	],"summary":"2 findings"}`

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepCI,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			// A complete fresh re-observation: everything is green now.
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	// One change fixed both checks, so the operator names only ci-1.
	if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-1"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the step re-parked on ci-2 even though its own fresh round observed everything green")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Fatalf("step status = %s, want completed", dbSteps[0].Status)
	}
	if dbSteps[0].FindingsJSON != nil {
		t.Fatalf("a complete-assessment round reported nothing, so nothing may remain outstanding; got %q", *dbSteps[0].FindingsJSON)
	}
}

// TestExecutor_CarriedFindingKeepsItsIDWhenTheRestatementMovesLine walks the
// executor path the identity rule protects: the agent re-reports an
// outstanding defect at its new line while also reporting a genuinely new
// one, and the new finding's positional ID collides with the carried one.
func TestExecutor_CarriedFindingKeepsItsIDWhenTheRestatementMovesLine(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","file":"a.go","line":10,"description":"bug A","action":"ask-user"},
		{"id":"review-2","severity":"warning","file":"c.go","line":1,"description":"typo","action":"auto-fix"}
	],"summary":"2 findings"}`

	// Round 2 reports a brand-new finding first (so it normalizes to
	// "review-1") and restates bug A at its shifted line.
	round2Findings := `{"findings":[
		{"severity":"warning","file":"b.go","line":5,"description":"bug C","action":"ask-user"},
		{"severity":"error","file":"a.go","line":12,"description":"bug A","action":"ask-user"}
	],"summary":"2 findings"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			return &StepOutcome{Findings: round2Findings}, nil
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
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	byDescription := map[string]string{}
	for _, item := range parsed.Items {
		if prior, ok := byDescription[item.Description]; ok {
			t.Fatalf("two findings share description %q (ids %q, %q)", item.Description, prior, item.ID)
		}
		byDescription[item.Description] = item.ID
	}
	if byDescription["bug A"] != "review-1" {
		t.Errorf("bug A was shown to the operator as review-1 and is still outstanding; it is now %q, so a respond naming review-1 would dispatch %q instead", byDescription["bug A"], "bug C")
	}
	if byDescription["bug C"] == "review-1" {
		t.Errorf("the brand-new finding inherited the carried identity: %#v", byDescription)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_ResumeCarriesUnselectedFindingsAcrossADaemonRestart covers the
// recovery path's own carry-forward. A daemon restart mid-gate must not
// reopen the hole the in-loop path closes: the parked step's findings are the
// union carried into that gate, and whatever the recovery response does not
// select is still unresolved when the resumed round reports nothing new.
func TestExecutor_ResumeCarriesUnselectedFindingsAcrossADaemonRestart(t *testing.T) {
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
	findings := `{"findings":[
		{"id":"review-1","severity":"warning","description":"selected issue","action":"ask-user"},
		{"id":"review-2","severity":"error","description":"unselected issue","action":"ask-user"}
	],"summary":"2 outstanding findings"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "1111111111111111111111111111111111111111", 25); err != nil {
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

	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			// The resumed round is scoped to the fix diff and finds nothing
			// new in it.
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted a response")
		}
		time.Sleep(10 * time.Millisecond)
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected the recovered step to still carry its unresolved finding")
	}
	items := mustParseFindingsWithAction(t, *dbSteps[0].FindingsJSON)
	byID := map[string]string{}
	for _, item := range items {
		byID[item.ID] = item.Action
	}
	if byID["review-2"] != "ask-user" {
		t.Fatalf("review-2 was never named in a respond action, so it must still block after recovery; got %#v", byID)
	}
	if _, stillThere := byID["review-1"]; stillThere {
		t.Errorf("review-1 was selected and fixed; it must not still be outstanding: %#v", byID)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

// TestExecutor_AutoFixDoesNotSweepUpAFindingTheOperatorLeftUnselected pins
// what carrying a finding forward does NOT authorize. Auto-fix eligibility is
// an offer a round makes about its own output at the gate where it was shown;
// once the operator answered that gate without selecting the finding, a later
// automatic round must not dispatch it anyway.
func TestExecutor_AutoFixDoesNotSweepUpAFindingTheOperatorLeftUnselected(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}}

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"blocker","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"selected nit","action":"auto-fix"},
		{"id":"review-3","severity":"warning","description":"declined nit","action":"auto-fix"}
	],"summary":"3 findings"}`

	var dispatched []string
	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			switch callCount {
			case 1:
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			default:
				dispatched = append(dispatched, sctx.PreviousFindings)
				return &StepOutcome{AutoFixable: true}, nil
			}
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	// review-1 is still outstanding, so the run re-parks rather than
	// completing; that is the observable point at which any auto-fix dispatch
	// for round 2 has already happened.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	for _, payload := range dispatched {
		if strings.Contains(payload, "declined nit") {
			t.Fatalf("review-3 was shown at the gate and deliberately left unselected; an auto-fix round must not dispatch it: %s", payload)
		}
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil || !strings.Contains(*dbSteps[0].FindingsJSON, "declined nit") {
		t.Fatalf("review-3 must remain outstanding at the gate, got %v", dbSteps[0].FindingsJSON)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_CarriedRoundsKeepTestEvidenceInTheStepFindings guards the
// evidence fields against the merge that runs on every carried round. The
// step here is shaped like the real ReviewStep - the only step that declares
// its rounds may be scope limited, and so the only one whose rounds reach
// mergeFindingsJSON at all.
//
// The exposure is that the merge is a whole-payload rebuild: a field it
// forgets to carry is gone from what the step persists. That matters because
// the PR body reads the step's merged findings alone - testingEvidenceFindingsJSON
// stops at sr.FindingsJSON as soon as it carries any testing metadata, and
// testing_summary always survives the merge - so evidence dropped here never
// reaches the PR body from the round records either.
func TestExecutor_CarriedRoundsKeepTestEvidenceInTheStepFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"flaky assertion","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"missing case","action":"auto-fix"}
	],"summary":"2 findings","tested":["login flow"],"testing_summary":"ran the targeted checks",
	"artifacts":[{"kind":"log","label":"first round log","path":"/tmp/first.log"}]}`

	// The fix round re-runs only the check it repaired and reports no
	// artifacts of its own.
	round2Findings := `{"findings":[],"summary":"0 findings","testing_summary":"re-ran the repaired check"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			return &StepOutcome{Findings: round2Findings}, nil
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
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(parsed.Artifacts) != 1 || parsed.Artifacts[0].Label != "first round log" {
		t.Fatalf("the fix round produced no artifacts of its own, so the earlier round's must survive into the step findings the PR body reads; got %#v", parsed.Artifacts)
	}
	if len(parsed.Tested) != 1 || parsed.Tested[0] != "login flow" {
		t.Fatalf("expected the earlier round's tested entries to survive, got %#v", parsed.Tested)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}

// TestExecutor_StripsFabricatedUserAcceptanceRationale is the DEFECT 1
// fabricated-approval regression: round 2's own generated risk_rationale
// claims the remaining ask-user findings were "explicitly accepted by the
// user". Nobody accepted them - the only respond action on record selected
// two unrelated findings for fix. That claim must not survive into stored or
// displayed output.
func TestExecutor_StripsFabricatedUserAcceptanceRationale(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"review-1","severity":"error","description":"copy is dishonest","action":"ask-user"},
		{"id":"review-2","severity":"warning","description":"typo","action":"auto-fix"}
	],"summary":"2 findings"}`

	const fabricatedRationale = "the remaining copy-honesty items were explicitly accepted by the user"
	round2Findings := `{"findings":[],"summary":"0 findings","risk_level":"low","risk_rationale":"` + fabricatedRationale + `"}`

	callCount := 0
	step := &adaptiveCallStep{
		name:                 types.StepReview,
		scopeLimitedFindings: true,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: round1Findings}, nil
			}
			return &StepOutcome{Findings: round2Findings}, nil
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

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON == nil {
		t.Fatal("expected findings to be persisted")
	}
	parsed, err := types.ParseFindingsJSON(*dbSteps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if strings.Contains(strings.ToLower(parsed.RiskRationale), "accepted by the user") {
		t.Fatalf("risk_rationale still asserts an uncorroborated user acceptance: %q", parsed.RiskRationale)
	}
	if parsed.RiskRationale == fabricatedRationale {
		t.Fatalf("fabricated rationale was persisted verbatim: %q", parsed.RiskRationale)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
}
