package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCollectValidationGaps(t *testing.T) {
	gaps := CollectValidationGaps(ValidationInput{
		Scenarios: []types.TestScenario{
			{Name: "checkout", Result: types.ScenarioResultPass, Live: true, Evidence: "drove it"},
			{Name: "refund", Result: types.ScenarioResultUntested, Reason: "needs a provider credential"},
		},
		Verdict:       types.TestVerdictGo,
		TestException: "test agent invocation budget exhausted",
		CIOverride:    "lint still red",
		Steps: []ValidationStep{
			{Name: "review", Status: string(types.StepStatusSkipped), SkipReason: types.OperatorSkipReason},
			{Name: "document", Status: string(types.StepStatusSkipped), SkipReason: types.OperatorSkipReason},
			{Name: "pr", Status: string(types.StepStatusSkipped), SkipReason: "gh is not authenticated"},
			{Name: "ci", Status: string(types.StepStatusSkipped), SkipReason: types.OperatorSkipReason + ": gh is not authenticated"},
			// Nothing to validate (empty diff after rebase) records no reason and is no gap.
			{Name: "lint", Status: string(types.StepStatusSkipped)},
			// A completed step's stale reason is ignored.
			{Name: "ci", Status: string(types.StepStatusCompleted), SkipReason: "stale"},
		},
	})
	if gaps.Verdict != types.TestVerdictGo || gaps.Live != 1 || gaps.Total != 2 {
		t.Fatalf("counts = %+v", gaps)
	}
	if len(gaps.Untested) != 1 || gaps.Untested[0] != (UntestedScenario{Scenario: "refund", Reason: "needs a provider credential"}) {
		t.Fatalf("untested = %+v", gaps.Untested)
	}
	want := []ValidationException{
		{Step: "test", Kind: ValidationKindTestException, Reason: "test agent invocation budget exhausted"},
		{Step: "ci", Kind: ValidationKindCIOverride, Reason: "lint still red"},
		{Step: "review", Kind: ValidationKindOperatorSkip, Reason: NoReasonRecorded},
		{Step: "document", Kind: ValidationKindOperatorSkip, Reason: NoReasonRecorded},
		{Step: "pr", Kind: ValidationKindAutomaticSkip, Reason: "gh is not authenticated"},
		{Step: "ci", Kind: ValidationKindAutomaticSkip, Reason: types.OperatorSkipReason + ": gh is not authenticated"},
	}
	if len(gaps.Exceptions) != len(want) {
		t.Fatalf("exceptions = %+v", gaps.Exceptions)
	}
	for i := range want {
		if gaps.Exceptions[i] != want[i] {
			t.Fatalf("exception %d = %+v, want %+v", i, gaps.Exceptions[i], want[i])
		}
	}
	if !gaps.HasGaps() || gaps.Empty() {
		t.Fatal("gaps not reported")
	}
}

func TestCollectValidationGaps_NothingToReport(t *testing.T) {
	gaps := CollectValidationGaps(ValidationInput{Verdict: "not-a-verdict"})
	if !gaps.Empty() || gaps.HasGaps() {
		t.Fatalf("gaps = %+v", gaps)
	}
	if IsAutomaticSkip("ci", types.OperatorSkipReason) || IsAutomaticSkip("review", "cause") || !IsAutomaticSkip("ci", "cause") || types.IsOperatorSkipReason(types.OperatorSkipReason+": gh is not authenticated") {
		t.Fatal("automatic skip classification is wrong")
	}
}

// Explicit operator skips are recorded apart from the empty-diff skip, which
// records no reason, so the validation block can list one and not the other.
func TestExecutor_ExplicitSkipsRecordTheOperatorMarker(t *testing.T) {
	database, p, run, repo := setupTest(t)
	review := newPassStep(types.StepReview)
	test := newApprovalStep(types.StepTest, `{"findings":[{"id":"t1","severity":"warning","description":"needs a decision","action":"ask-user"}]}`)
	document := newPassStep(types.StepDocument)
	executor := NewExecutor(database, p, nil, nil, []Step{review, test, document}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepDocument})
	done, _ := startExecutor(t, executor, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if err := executor.Respond(types.StepTest, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[types.StepName]string{}
	for _, row := range rows {
		if row.SkipReason != nil {
			reasons[row.StepName] = *row.SkipReason
		}
	}
	if reasons[types.StepTest] != types.OperatorSkipReason || reasons[types.StepDocument] != types.OperatorSkipReason {
		t.Fatalf("skip reasons = %+v, want the operator marker on test and document", reasons)
	}
	if _, ok := reasons[types.StepReview]; ok {
		t.Fatalf("completed review recorded a skip reason: %+v", reasons)
	}

	empty, err := database.InsertRun(repo.ID, run.Branch, run.HeadSHA, run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	rebase := newPassStep(types.StepRebase)
	rebase.outcome.SkipRemaining = true
	if err := NewExecutor(database, p, nil, nil, []Step{rebase, newPassStep(types.StepReview)}, nil).Execute(t.Context(), empty, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	rows, err = database.GetStepsByRun(empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepReview && (row.Status != types.StepStatusSkipped || row.SkipReason != nil) {
			t.Fatalf("empty-diff skip = %s %v, want skipped with no reason", row.Status, row.SkipReason)
		}
	}
}
