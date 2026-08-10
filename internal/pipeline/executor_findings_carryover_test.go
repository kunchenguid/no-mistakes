package pipeline

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

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
		name: types.StepReview,
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
		name: types.StepReview,
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
		name: types.StepReview,
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
		name: types.StepReview,
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

// TestExecutor_CarriedRoundsKeepTestEvidenceInTheStepFindings guards the test
// step's evidence across a fix round. The PR body reads the step's merged
// findings alone - testingEvidenceFindingsJSON stops at sr.FindingsJSON as
// soon as it carries any testing metadata, and testing_summary always
// survives the merge - so an artifact dropped while unioning the carried set
// never reaches the PR body from the round records either.
func TestExecutor_CarriedRoundsKeepTestEvidenceInTheStepFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	round1Findings := `{"findings":[
		{"id":"test-1","severity":"error","description":"flaky assertion","action":"ask-user"},
		{"id":"test-2","severity":"warning","description":"missing case","action":"auto-fix"}
	],"summary":"2 findings","tested":["login flow"],"testing_summary":"ran the targeted checks",
	"artifacts":[{"kind":"log","label":"first round log","path":"/tmp/first.log"}]}`

	// The fix round re-runs only the check it repaired and reports no
	// artifacts of its own.
	round2Findings := `{"findings":[],"summary":"0 findings","testing_summary":"re-ran the repaired check"}`

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
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

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepTest, types.ActionFix, []string{"test-2"}); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusFixReview)

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

	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
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
		name: types.StepReview,
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
