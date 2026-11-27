package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func exceptionsFixture() []*db.StepResult {
	noGo := `{"findings":[{"id":"test-1","severity":"error","description":"refund fails"}],"verdict":"no-go"}`
	approval := "provider sandbox is down; refund path reviewed by hand"
	skip := "lint runs in the forge's own CI"
	return []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &noGo, ApprovalReason: &approval},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
		{ID: "lint", StepName: types.StepLint, Status: types.StepStatusSkipped, OperatorSkipReason: &skip},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusSkipped, SkipReason: ptrString("no PR host")},
	}
}

func ptrString(s string) *string { return &s }

// The attestation lists the Test exception and the operator skip with the
// reasons the operator recorded, in step order, and nothing for an automatic
// skip or an ordinary completion.
func TestPipelineAttestationListsOperatorExceptionsWithTheirReasons(t *testing.T) {
	t.Parallel()
	got := parsePipelineAttestationForTest(t, buildPipelineAttestation(exceptionsFixture(), nil, testPipelineHeadSHA)).Exceptions
	want := []pipelineAttestationException{
		{Step: types.StepTest, Kind: "test-exception", Condition: "live validation verdict: no-go", Reason: "provider sandbox is down; refund path reviewed by hand"},
		{Step: types.StepLint, Kind: "operator-skip", Reason: "lint runs in the forge's own CI"},
	}
	if len(got) != len(want) {
		t.Fatalf("exceptions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exceptions[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A run with no operator decision publishes no exceptions field at all, so
// its attestation is byte-for-byte what it was before the field existed.
func TestPipelineAttestationOmitsExceptionsWhenThereAreNone(t *testing.T) {
	t.Parallel()
	body := compliantPipelineBody(t, testPipelineHeadSHA)
	if strings.Contains(body, `"exceptions"`) {
		t.Fatalf("a clean run published an exceptions field:\n%s", body)
	}
}

// A decision recorded without a reason - an older record, or a surface that
// takes none - is listed with the fallback reason rather than dropped.
func TestPipelineAttestationListsAReasonlessDecisionWithTheFallback(t *testing.T) {
	t.Parallel()
	noGo := `{"findings":[],"verdict":"inconclusive"}`
	blank := ""
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusSkipped, OperatorSkipReason: &blank},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &noGo, ApprovalReason: &blank},
	}
	got := parsePipelineAttestationForTest(t, buildPipelineAttestation(steps, nil, testPipelineHeadSHA)).Exceptions
	if len(got) != 2 || got[0].Reason != db.NoOperatorReasonSupplied || got[1].Reason != db.NoOperatorReasonSupplied {
		t.Fatalf("exceptions = %+v, want both with the fallback reason", got)
	}
}

// A CI repair that restamps the head without re-running the steps keeps the
// exceptions the attestation carried, reasons included; a restamp with the
// run's current steps derives them afresh.
func TestRebindPipelineAttestationCarriesExceptions(t *testing.T) {
	t.Parallel()
	original := buildPipelineAttestation(exceptionsFixture(), nil, testPipelineHeadSHA)
	repairHead := strings.Repeat("ab", 20)

	rebound, ok := rebindPipelineAttestationHead(original, repairHead)
	if !ok {
		t.Fatal("expected a live attestation to rebind")
	}
	before := parsePipelineAttestationForTest(t, original).Exceptions
	after := parsePipelineAttestationForTest(t, rebound).Exceptions
	if len(after) != len(before) || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("restamp exceptions = %+v, want the carried %+v", after, before)
	}

	current := []*db.StepResult{{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted}}
	refreshed, ok := rebindPipelineAttestationWithSteps(original, repairHead, current, pipelineAttestationPolicy{})
	if !ok {
		t.Fatal("expected a live attestation to rebind")
	}
	if got := parsePipelineAttestationForTest(t, refreshed).Exceptions; len(got) != 0 {
		t.Fatalf("a restamp with current steps kept stale exceptions: %+v", got)
	}
}

// The field is additive: require-no-mistakes reaches the same verdict, and a
// reason with a control byte still publishes as valid JSON.
func TestPipelineAttestationExceptionsLeaveVerifyPyUnchanged(t *testing.T) {
	t.Parallel()
	steps := exceptionsFixture()
	reason := "operator reason \x1b[31m with a control byte"
	steps[3].OperatorSkipReason = &reason
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	body := redactPRContent(prContent{Body: md}, scm.ProviderGitHub, "", "").Body
	if body != md {
		t.Fatalf("the publication boundary changed the attestation:\n%s\n%s", md, body)
	}
	if len(parsePipelineAttestationForTest(t, body).Exceptions) != 2 {
		t.Fatalf("published attestation lost its exceptions:\n%s", body)
	}
	if got, out := runVerifyPy(t, body, testPipelineHeadSHA); got != "success" {
		t.Fatalf("verify.py = %s with an exceptions field\n%s", got, out)
	}
}
