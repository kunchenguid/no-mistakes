package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The finding at the centre of the drift this channel prevents: a review asked
// for a design change, the human declined it, and a later step re-applied it
// anyway because the decision never reached that step.
const declinedDedupFindings = `{"findings":[{"id":"journal-version-deduplication","severity":"error",` +
	`"file":"tools/release_announcement_action.js","line":497,` +
	`"description":"dedup checks only the last event; check all prior events instead","action":"ask-user"}]}`

type decisionFixture struct {
	db       *db.DB
	repo     *db.Repo
	run      *db.Run
	reviewSR *db.StepResult
	testSR   *db.StepResult
}

func newDecisionFixture(t *testing.T) *decisionFixture {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head1", "base")
	if err != nil {
		t.Fatal(err)
	}
	reviewSR, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	testSR, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	return &decisionFixture{db: database, repo: repo, run: run, reviewSR: reviewSR, testSR: testSR}
}

// declineReviewRound records the review raising findings and the human
// resolving the gate without selecting any of them, exactly as the executor's
// approve, skip, and abort paths do.
func (f *decisionFixture) declineReviewRound(t *testing.T, findings string) {
	t.Helper()
	round, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetStepRoundDeclined(round.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *decisionFixture) testStepContext() *pipeline.StepContext {
	return &pipeline.StepContext{
		DB:           f.db,
		Repo:         f.repo,
		Run:          f.run,
		StepResultID: f.testSR.ID,
	}
}

// The core regression: a finding the human declined in the review step must be
// visible to a later step in the same run, so that step does not re-implement
// it. Before this channel existed the test step's prompt carried no trace of
// the decision at all.
func TestDeclinedFindingReachesALaterStepInTheSameRun(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	got := roundHistoryPromptSection(f.testStepContext())

	if !strings.Contains(got, "journal-version-deduplication") {
		t.Fatalf("test step cannot see the declined review finding:\n%s", got)
	}
	if !strings.Contains(got, "Decisions already made by the user in this run") {
		t.Fatalf("missing the in-run decision section:\n%s", got)
	}
	if !strings.Contains(got, "review round 1 declined:") {
		t.Fatalf("the decision is not attributed to the review step:\n%s", got)
	}
	// The precedence rule is the load-bearing instruction: the decision is
	// usually the human resolving an ambiguity in the intent prose, so a step
	// must not "fix" the code back to what the prose seems to say.
	if !strings.Contains(got, "SUPERSEDES") {
		t.Fatalf("missing the intent-precedence instruction:\n%s", got)
	}
	if !strings.Contains(got, "Do NOT implement them") {
		t.Fatalf("missing the do-not-implement instruction:\n%s", got)
	}
}

// A decision made on this branch must still stand for a later run. The
// uncertified-range channel could not do this because completing a review
// deletes it; a decision has no such expiry.
func TestDeclinedFindingReachesALaterRunOnTheSameBranch(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	laterRun, err := f.db.InsertRun(f.repo.ID, "feature", "head2", "head1")
	if err != nil {
		t.Fatal(err)
	}
	laterTestSR, err := f.db.InsertStepResult(laterRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{
		DB:           f.db,
		Repo:         f.repo,
		Run:          laterRun,
		StepResultID: laterTestSR.ID,
	}
	pipeline.BindBranchDecisions(sctx)

	got := roundHistoryPromptSection(sctx)
	if !strings.Contains(got, "journal-version-deduplication") {
		t.Fatalf("the later run cannot see the prior decision:\n%s", got)
	}
	if !strings.Contains(got, "earlier runs") {
		t.Fatalf("missing the cross-run decision section:\n%s", got)
	}
	if !strings.Contains(got, "Entries are chronological") {
		t.Fatalf("missing the decision-history ordering statement:\n%s", got)
	}
}

func TestSameRunChoiceToFixSupersedesEarlierStepDecline(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	currentRun, err := f.db.InsertRun(f.repo.ID, "feature", "head2", "head1")
	if err != nil {
		t.Fatal(err)
	}
	reviewStep, err := f.db.InsertStepResult(currentRun.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	testStep, err := f.db.InsertStepResult(currentRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	documentStep, err := f.db.InsertStepResult(currentRun.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}

	findings := `{"findings":[{"id":"same-run-reversal","severity":"error","description":"same concern","action":"ask-user"}]}`
	reviewRound, err := f.db.InsertStepRound(reviewStep.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetStepRoundDeclined(reviewRound.ID); err != nil {
		t.Fatal(err)
	}
	testRound, err := f.db.InsertStepRound(testStep.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["same-run-reversal"]`
	if err := f.db.SetStepRoundUserDecision(testRound.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	ownFindings := `{"findings":[{"id":"current-step-history","severity":"warning","description":"current","action":"auto-fix"}]}`
	if _, err := f.db.InsertStepRound(documentStep.ID, 1, "initial", &ownFindings, nil, 10); err != nil {
		t.Fatal(err)
	}

	sctx := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: currentRun, StepResultID: documentStep.ID}
	pipeline.BindBranchDecisions(sctx)
	got := roundHistoryPromptSection(sctx)

	declinedAt := strings.Index(got, "review round 1 declined: {\"id\":\"same-run-reversal\"")
	fixedAt := strings.Index(got, "test round 1 user chose to fix: {\"id\":\"same-run-reversal\"")
	if declinedAt < 0 || fixedAt < 0 {
		t.Fatalf("expected both same-run decisions to remain visible:\n%s", got)
	}
	if fixedAt <= declinedAt {
		t.Fatalf("later same-run chose-to-fix decision must follow the earlier decline:\n%s", got)
	}
	branchAt := strings.Index(got, "earlier runs")
	runAt := strings.Index(got, "in this run")
	currentAt := strings.Index(got, "Previous rounds for this step")
	if branchAt < 0 || runAt <= branchAt || currentAt <= runAt {
		t.Fatalf("decision histories must render oldest first across runs, steps, and rounds:\n%s", got)
	}
	if !strings.Contains(got, "LATER entry about the same concern supersedes an earlier entry") {
		t.Fatalf("missing chronological precedence rule:\n%s", got)
	}
}

func TestLaterChoiceToFixSupersedesEarlierDeclineWithoutHidingIt(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	fixRun, err := f.db.InsertRun(f.repo.ID, "feature", "head2", "head1")
	if err != nil {
		t.Fatal(err)
	}
	fixStep, err := f.db.InsertStepResult(fixRun.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := declinedDedupFindings
	fixRound, err := f.db.InsertStepRound(fixStep.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["journal-version-deduplication"]`
	if err := f.db.SetStepRoundUserDecision(fixRound.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}

	currentRun, err := f.db.InsertRun(f.repo.ID, "feature", "head3", "head2")
	if err != nil {
		t.Fatal(err)
	}
	currentStep, err := f.db.InsertStepResult(currentRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: currentRun, StepResultID: currentStep.ID}
	pipeline.BindBranchDecisions(sctx)

	got := roundHistoryPromptSection(sctx)
	declinedAt := strings.Index(got, "review round 1 declined:")
	fixedAt := strings.Index(got, "review round 1 user chose to fix:")
	if declinedAt < 0 || fixedAt < 0 {
		t.Fatalf("expected both contrary decisions to remain visible:\n%s", got)
	}
	if fixedAt <= declinedAt {
		t.Fatalf("newer chose-to-fix decision must follow the older decline:\n%s", got)
	}
	if !strings.Contains(got, "LATER entry about the same concern supersedes an earlier entry") {
		t.Fatalf("missing chronological precedence rule:\n%s", got)
	}
}

// Completing a review clears the uncertified pipeline range. It must not clear
// a recorded decision: approving the gate IS the decision, so an approval that
// erased it would be self-defeating.
func TestCompletedReviewDoesNotClearBranchDecisions(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	if err := f.db.CompleteReviewStep(f.reviewSR.ID, f.run.ID, "head1", 0, 5, ""); err != nil {
		t.Fatal(err)
	}

	laterRun, err := f.db.InsertRun(f.repo.ID, "feature", "head2", "head1")
	if err != nil {
		t.Fatal(err)
	}
	decisions, truncated, err := f.db.GetBranchDecisionRounds(f.repo.ID, "feature", laterRun.ID, db.MaxBranchDecisionRounds)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("one branch decision must not report truncation")
	}
	if len(decisions) != 1 {
		t.Fatalf("a completed review erased the decision: got %d", len(decisions))
	}
}

// A partial selection is also a decision: the findings the user did not pick
// are declined, and that half must travel too.
func TestPartiallySelectedRoundCarriesItsDeclinedHalfAcrossSteps(t *testing.T) {
	f := newDecisionFixture(t)
	findings := `{"findings":[` +
		`{"id":"fix-me","severity":"warning","description":"typo","action":"auto-fix"},` +
		`{"id":"keep-a","severity":"error","description":"switch to B","action":"ask-user"}]}`
	round, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["fix-me"]`
	if err := f.db.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}

	got := roundHistoryPromptSection(f.testStepContext())
	if !strings.Contains(got, `review round 1 declined: {"id":"keep-a"`) {
		t.Fatalf("the declined half did not cross the step boundary under the declined label:\n%s", got)
	}
	if strings.Contains(got, `review round 1 user chose to fix: {"id":"keep-a"`) {
		t.Fatalf("a declined finding must not be reported as chosen to fix:\n%s", got)
	}
	if !strings.Contains(got, `review round 1 user chose to fix: {"id":"fix-me"`) {
		t.Fatalf("the selected half did not cross the step boundary under the chose-to-fix label:\n%s", got)
	}
	if strings.Contains(got, `review round 1 declined: {"id":"fix-me"`) {
		t.Fatalf("a finding the user chose to fix must not be reported as declined:\n%s", got)
	}
}

// An auto-fix selection is a filter's choice, not a human's. Its complement is
// findings still awaiting a decision, so presenting them as declined would
// suppress a finding nobody has ruled on.
func TestAutoFixComplementIsNeverPresentedAsAUserDecision(t *testing.T) {
	f := newDecisionFixture(t)
	findings := `{"findings":[` +
		`{"id":"auto-me","severity":"warning","description":"typo","action":"auto-fix"},` +
		`{"id":"pending-decision","severity":"error","description":"design choice","action":"ask-user"}]}`
	round, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["auto-me"]`
	if err := f.db.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}

	crossStep := roundHistoryPromptSection(f.testStepContext())
	if strings.Contains(crossStep, "pending-decision") {
		t.Fatalf("an undecided finding leaked into another step as a decision:\n%s", crossStep)
	}

	// Within the step that produced it, the complement is still surfaced so a
	// fix agent knows those findings exist, but under its own label and
	// without the do-not-re-report instruction.
	reviewCtx := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: f.run, StepResultID: f.reviewSR.ID}
	sameStep := roundHistoryPromptSection(reviewCtx)
	if !strings.Contains(sameStep, "auto_fix_left_unselected:") {
		t.Fatalf("the auto-fix complement is not surfaced to its own step:\n%s", sameStep)
	}
	if !strings.Contains(sameStep, "pending-decision") {
		t.Fatalf("the unselected finding is missing from its own step's history:\n%s", sameStep)
	}
	if strings.Contains(sameStep, "\nuser_chose_to_ignore:") {
		t.Fatalf("an auto-fix complement must not be labelled a user decline:\n%s", sameStep)
	}
}

// A declined round renders only the declined half: the selection is an
// explicit empty set, so there is no "chose to fix" list to show.
func TestDeclinedRoundRendersOnlyTheIgnoredHalfInItsOwnStep(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	reviewCtx := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: f.run, StepResultID: f.reviewSR.ID}
	got := roundHistoryPromptSection(reviewCtx)
	if !strings.Contains(got, "\nuser_chose_to_ignore:") {
		t.Fatalf("expected the declined half, got:\n%s", got)
	}
	if strings.Contains(got, "\nuser_chose_to_fix:") {
		t.Fatalf("a decline must not render an empty chose-to-fix list:\n%s", got)
	}
}

// A round with no recorded decision must produce no decision section, so an
// unresolved finding is never presented as settled.
func TestUnresolvedRoundProducesNoDecisionSection(t *testing.T) {
	f := newDecisionFixture(t)
	findings := declinedDedupFindings
	if _, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}

	got := roundHistoryPromptSection(f.testStepContext())
	if strings.Contains(got, "Decisions already made") {
		t.Fatalf("an unresolved round was rendered as a decision:\n%s", got)
	}
}

// The sections must degrade to nothing when the context carries no run or
// repo, so a step context built without them keeps working.
func TestDecisionSectionsAreAbsentWithoutRunOrRepo(t *testing.T) {
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)

	noRun := &pipeline.StepContext{DB: f.db, StepResultID: f.testSR.ID}
	if got := roundHistoryPromptSection(noRun); got != "" {
		t.Fatalf("expected no section without a run, got:\n%s", got)
	}

	sctx := &pipeline.StepContext{DB: f.db, StepResultID: f.testSR.ID}
	pipeline.BindBranchDecisions(sctx)
	if len(sctx.PriorBranchDecisions) != 0 {
		t.Fatal("binding without a repo or run must load nothing")
	}
}

// Prompt budget: a long decision history renders its most recent entries and
// says what it dropped, so a truncated history never reads as a complete one.
func TestDecisionSectionBoundsALongHistory(t *testing.T) {
	f := newDecisionFixture(t)
	var items []string
	for i := 0; i < maxDecisionLinesPerSection+5; i++ {
		items = append(items, `{"id":"finding-`+string(rune('a'+i%26))+string(rune('a'+i/26))+
			`","severity":"error","description":"d","action":"ask-user"}`)
	}
	f.declineReviewRound(t, `{"findings":[`+strings.Join(items, ",")+`]}`)

	got := roundHistoryPromptSection(f.testStepContext())
	if !strings.Contains(got, "older decision finding(s) omitted for length") {
		t.Fatalf("expected a truncation note:\n%s", got)
	}
	if n := strings.Count(got, "review round 1 declined:"); n != maxDecisionLinesPerSection {
		t.Fatalf("rendered %d declined lines, want the bound %d", n, maxDecisionLinesPerSection)
	}
}

func TestBranchDecisionLoaderTruncationIsVisible(t *testing.T) {
	f := newDecisionFixture(t)
	for i := 0; i < db.MaxBranchDecisionRounds+1; i++ {
		run, err := f.db.InsertRun(f.repo.ID, "feature", fmt.Sprintf("prior-head-%d", i), "base")
		if err != nil {
			t.Fatal(err)
		}
		step, err := f.db.InsertStepResult(run.ID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		findings := fmt.Sprintf(`{"findings":[{"id":"prior-%d","severity":"error","description":"d","action":"ask-user"}]}`, i)
		round, err := f.db.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.SetStepRoundDeclined(round.ID); err != nil {
			t.Fatal(err)
		}
	}

	currentRun, err := f.db.InsertRun(f.repo.ID, "feature", "current-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	currentStep, err := f.db.InsertStepResult(currentRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: currentRun, StepResultID: currentStep.ID}
	pipeline.BindBranchDecisions(sctx)

	if len(sctx.PriorBranchDecisions) != db.MaxBranchDecisionRounds {
		t.Fatalf("loaded %d branch decisions, want %d", len(sctx.PriorBranchDecisions), db.MaxBranchDecisionRounds)
	}
	if !sctx.PriorBranchDecisionsTruncated {
		t.Fatal("bounded branch decisions did not retain truncation metadata")
	}
	got := branchDecisionsPromptSection(sctx)
	if !strings.Contains(got, "Older branch decision round(s) omitted by the history limit") {
		t.Fatalf("rendered bounded history did not disclose omitted rounds:\n%s", got)
	}
}

func TestDecisionSectionBoundsOneOversizedFinding(t *testing.T) {
	f := newDecisionFixture(t)
	findings := `{"findings":[{"id":"huge","severity":"error","description":"` +
		strings.Repeat("x", maxDecisionSectionBytes*4) + `","action":"ask-user"}]}`
	f.declineReviewRound(t, findings)

	got := runDecisionsPromptSection(f.testStepContext())
	if len(got) > maxDecisionSectionBytes {
		t.Fatalf("decision section is %d bytes, want at most %d", len(got), maxDecisionSectionBytes)
	}
	if !strings.Contains(got, "overlong decision finding(s) truncated") {
		t.Fatalf("expected a named truncation note:\n%s", got)
	}
	if !strings.Contains(got, "huge") {
		t.Fatalf("truncation removed the finding identity:\n%s", got)
	}
}

// The decision channel exists so the steps that run after a human rules on
// review findings cannot silently undo or re-apply what was decided. These
// tests exercise the real rendered step prompts - not just the section helper
// - and pin both halves of the contract: the recorded decisions (which
// findings the human chose to fix or declined, with their instructions) and
// the explicit instruction to respect them and never revert or undo them.
const stepDecisionsFindings = `{"findings":[` +
	`{"id":"chosen-fix","severity":"error","file":"a.go","line":3,` +
	`"description":"rename the journal field to entry","action":"ask-user",` +
	`"user_instructions":"rename it to entry, not record"},` +
	`{"id":"declined-redesign","severity":"warning","file":"b.go","line":9,` +
	`"description":"redesign the dedup loop entirely","action":"ask-user",` +
	`"user_instructions":"too risky for this change"}]}`

// recordHumanReviewDecision seeds a review round on the context's run that the
// human resolved by selecting chosen-fix, leaving declined-redesign as the
// declined half.
func recordHumanReviewDecision(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	reviewSR, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := stepDecisionsFindings
	round, err := sctx.DB.InsertStepRound(reviewSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["chosen-fix"]`
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
}

// bindStepResult registers the step under test as its own step result so the
// cross-step decision channel treats the seeded review decision as another
// step's, exactly as the executor wires it.
func bindStepResult(t *testing.T, sctx *pipeline.StepContext, stepName types.StepName) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, stepName)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
}

// requireRecordedDecisionsInPrompt asserts the rendered agent prompt carries
// the seeded review decisions with their instructions and the
// respect-and-never-revert instruction.
func requireRecordedDecisionsInPrompt(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		`review round 1 user chose to fix: {"id":"chosen-fix"`,
		`"user_instructions":"rename it to entry, not record"`,
		`review round 1 declined: {"id":"declined-redesign"`,
		`"user_instructions":"too risky for this change"`,
		"Do NOT implement them",
		"A recorded decision SUPERSEDES conflicting user-intent wording",
		"Never revert, undo, or work around a recorded human decision",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing recorded-decision content %q:\n%s", want, prompt)
		}
	}
}

func TestDocumentPromptCarriesRecordedHumanDecisions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	recordHumanReviewDecision(t, sctx)
	bindStepResult(t, sctx, types.StepDocument)

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one document agent call, got %d", len(ag.calls))
	}
	requireRecordedDecisionsInPrompt(t, ag.calls[0].Prompt)
}

func TestLintAgentPassPromptCarriesRecordedHumanDecisions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	recordHumanReviewDecision(t, sctx)
	bindStepResult(t, sctx, types.StepLint)

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one lint agent call, got %d", len(ag.calls))
	}
	requireRecordedDecisionsInPrompt(t, ag.calls[0].Prompt)
}

func TestLintFixPromptCarriesRecordedHumanDecisions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"fixed lint"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	recordHumanReviewDecision(t, sctx)
	bindStepResult(t, sctx, types.StepLint)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"gofmt drift","action":"auto-fix"}]}`

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one lint fix agent call, got %d", len(ag.calls))
	}
	requireRecordedDecisionsInPrompt(t, ag.calls[0].Prompt)
}

// Both Test agent turns - the repair turn and the evidence turn - must carry
// the recorded decisions, since each can change the tree.
func TestTestPromptsCarryRecordedHumanDecisions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	call := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		call++
		if call == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fixed tests"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ok","scenarios":[{"name":"s","result":"pass","live":true,"evidence":"e","reason":"drove it"}],"verdict":"go","tested":["x"],"artifacts":[],"testing_summary":"ok"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	recordHumanReviewDecision(t, sctx)
	bindStepResult(t, sctx, types.StepTest)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"failing case","action":"auto-fix"}]}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("expected a repair turn and an evidence turn, got %d calls", len(ag.calls))
	}
	requireRecordedDecisionsInPrompt(t, ag.calls[0].Prompt)
	requireRecordedDecisionsInPrompt(t, ag.calls[1].Prompt)
}

// A step's own earlier gate decisions bind its later rounds just as another
// step's do: a fix turn must not undo a fix the human chose at this step's
// earlier gate.
func TestOwnStepHistoryPromptCarriesNeverRevertClause(t *testing.T) {
	f := newDecisionFixture(t)
	findings := `{"findings":[` +
		`{"id":"kept-fix","severity":"error","description":"keep the rename","action":"auto-fix"},` +
		`{"id":"skipped-nit","severity":"warning","description":"style nit","action":"ask-user"}]}`
	round, err := f.db.InsertStepRound(f.testSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["kept-fix"]`
	if err := f.db.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}

	got := stepRoundHistorySection(f.testStepContext())
	if !strings.Contains(got, "user_chose_to_fix:") || !strings.Contains(got, "kept-fix") {
		t.Fatalf("own-step chose-to-fix entry missing:\n%s", got)
	}
	if !strings.Contains(got, "user_chose_to_ignore:") || !strings.Contains(got, "skipped-nit") {
		t.Fatalf("own-step declined entry missing:\n%s", got)
	}
	if !strings.Contains(got, "Do NOT revert or undo fixes the user chose under user_chose_to_fix") {
		t.Fatalf("missing the never-revert clause for the step's own decisions:\n%s", got)
	}
}
