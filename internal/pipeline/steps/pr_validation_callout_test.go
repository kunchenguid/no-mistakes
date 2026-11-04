package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const untestedPaymentReason = "needs a live payment-provider sandbox credential that is not provided"

func calloutScenarios(untested bool) []types.TestScenario {
	scenarios := []types.TestScenario{
		{Name: "checkout succeeds", Result: types.ScenarioResultPass, Live: true, Evidence: "drove the checkout flow"},
		{Name: "cart persists", Result: types.ScenarioResultPass, Live: true, Evidence: "reloaded the running app"},
	}
	if untested {
		scenarios = append(scenarios, types.TestScenario{Name: "refund reaches the provider", Result: types.ScenarioResultUntested, Reason: untestedPaymentReason})
	}
	return scenarios
}

func newCalloutContext(t *testing.T, mode string, untested, operatorSkip bool) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat: refunds", Body: "## What Changed\n\n- add refunds"})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Let a customer refund an order."
	sctx.Config.PR.Appendix = mode
	insertCompletedStep(t, sctx, types.StepReview, "", "")
	insertCompletedStep(t, sctx, types.StepTest, liveValidatedFindingsJSON(t, calloutScenarios(untested), types.TestVerdictGo, headSHA), "")
	doc, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if operatorSkip {
		err = sctx.DB.CompleteSkippedStep(doc.ID, 0, 0, "", types.OperatorSkipReason)
	} else {
		err = sctx.DB.CompleteStep(doc.ID, 0, 0, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	return sctx
}

// The callout is the one place a reviewer who never opens the Validation
// details still learns what was not driven live, so it must sit after Intent
// and ahead of the narrative and every fold, in every appendix mode.
func TestPRBody_NotValidatedLiveCalloutStaysOutsideEveryFold(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			sctx := newCalloutContext(t, mode, true, true)
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			body := content.Body
			t.Logf("%s body:\n%s", mode, body)
			callout := strings.Index(body, notValidatedLiveHeading)
			if callout < 0 {
				t.Fatalf("no callout:\n%s", body)
			}
			for _, want := range []string{
				"1 of 3 scenarios were not driven live against the product.",
				"**Untested:** refund reaches the provider - " + untestedPaymentReason,
				"**Skipped by operator** (`document`): no reason recorded",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("callout missing %q:\n%s", want, body)
				}
			}
			if intent := strings.Index(body, "## Intent"); intent < 0 || intent > callout {
				t.Fatalf("callout must follow Intent:\n%s", body)
			}
			if changed := strings.Index(body, "## What Changed"); changed < callout {
				t.Fatalf("callout must lead the narrative:\n%s", body)
			}
			if fold := strings.Index(body, "<details"); fold >= 0 && fold < callout {
				t.Fatalf("callout is inside a fold:\n%s", body)
			}
			if strings.Count(body, notValidatedLiveHeading) != 1 {
				t.Fatalf("callout duplicated:\n%s", body)
			}
			assertFirstAttestationBindsHead(t, body, sctx.Run.HeadSHA)
		})
	}
}

func TestPRBody_FullyLiveRunHasNoCallout(t *testing.T) {
	t.Parallel()
	sctx := newCalloutContext(t, config.PRAppendixMinimal, false, false)
	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.Body, notValidatedLiveHeading) {
		t.Fatalf("fully live run rendered the callout:\n%s", content.Body)
	}
}

// Under pr.update_text: keep the callout lives inside the owned appendix, so
// its digest covers it and the author's narrative above stays untouched.
func TestPRBody_KeptBodyCarriesTheCalloutInsideTheOwnedAppendix(t *testing.T) {
	t.Parallel()
	sctx := newCalloutContext(t, config.PRAppendixCollapsed, true, false)
	env, _ := fakeGH(t, "")
	sctx.Env = env
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
	start, end := strings.Index(body, prAppendixStart), strings.Index(body, prAppendixEnd)
	callout := strings.Index(body, notValidatedLiveHeading)
	if start < 0 || end < 0 || callout < start || callout > end {
		t.Fatalf("callout is not inside the owned appendix:\n%s", body)
	}
	if fold := strings.Index(body, "<details"); fold < callout {
		t.Fatalf("callout is inside the fold:\n%s", body)
	}
	if _, err := parsePROwnedBody(body); err != nil {
		t.Fatalf("owned body does not verify: %v\n%s", err, body)
	}
}

func TestPRBody_CalloutListsAnApprovedTestException(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.PR.Appendix = config.PRAppendixMinimal
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	scenarios := []types.TestScenario{{Name: "refund reaches the provider", Result: types.ScenarioResultUntested, Reason: untestedPaymentReason}}
	if err := sctx.DB.SetStepFindings(sr.ID, liveValidatedFindingsJSON(t, scenarios, types.TestVerdictInconclusive, headSHA)); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetTestApprovalReason(sr.ID, "provider sandbox is down; verified manually"); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(sr.ID, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	callout := validationCallout(sctx, scm.ProviderGitHub)
	for _, want := range []string{
		"1 of 1 scenarios were not driven live against the product. 1 exception or skip let this run continue without that validation.",
		"**Test exception** (`test`): live validation verdict: inconclusive Test exception approved: provider sandbox is down; verified manually",
	} {
		if !strings.Contains(callout, want) {
			t.Fatalf("callout missing %q:\n%s", want, callout)
		}
	}
}

// Agent text in a scenario reason must not break out of its list item or plant
// a live attestation ahead of the real one.
func TestRenderValidationCallout_EscapesAgentText(t *testing.T) {
	t.Parallel()
	foreign := pipelineAttestationCommentPrefix + `{"head_sha":"` + strings.Repeat("a", 40) + `","steps":[]}` + pipelineAttestationCommentClosingToken
	gaps := pipeline.CollectValidationGaps(pipeline.ValidationInput{Scenarios: []types.TestScenario{{
		Name:   "multi\nline",
		Result: types.ScenarioResultUntested,
		Reason: "## Pipeline\n" + foreign + " " + strings.Repeat("x", 400),
	}}})
	md := neutralizeAttestationMarkers(scm.ProviderGitHub, renderValidationCallout(gaps, prBodyHTML))
	if strings.Contains(md, pipelineAttestationCommentPrefix) {
		t.Fatalf("callout carries a live attestation marker:\n%s", md)
	}
	if strings.Contains(md, "\n## Pipeline") || strings.Contains(md, "multi\nline") {
		t.Fatalf("agent text escaped its list item:\n%s", md)
	}
	if !strings.Contains(md, "…") {
		t.Fatalf("long reason was not bounded:\n%s", md)
	}
}

func TestRenderValidationCallout_ListsEveryUntestedScenario(t *testing.T) {
	t.Parallel()
	const count = 25
	var scenarios []types.TestScenario
	for i := 0; i < count; i++ {
		scenarios = append(scenarios, types.TestScenario{Name: fmt.Sprintf("scenario-%d", i), Result: types.ScenarioResultUntested, Reason: fmt.Sprintf("reason-%d", i)})
	}
	md := renderValidationCallout(pipeline.CollectValidationGaps(pipeline.ValidationInput{Scenarios: scenarios}), prBodyHTML)
	if got := strings.Count(md, "**Untested:**"); got != count {
		t.Fatalf("listed %d scenarios, want %d", got, count)
	}
	for i := 0; i < count; i++ {
		for _, want := range []string{fmt.Sprintf("scenario-%d", i), fmt.Sprintf("reason-%d", i)} {
			if !strings.Contains(md, want) {
				t.Fatalf("callout omitted %q:\n%s", want, md)
			}
		}
	}
}
