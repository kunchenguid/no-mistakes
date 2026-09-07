package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The Test step's live-validation verdict rides the PR-body attestation as a
// live_validation object, so a consumer asking "was this change validated
// against the real product?" reads a field instead of the Testing prose.
//
// These tests run the real body through the real gate script, because the
// attestation is a shared wire contract between this repository's renderer and
// every enforcing repository's copy of the action: a field added on one side
// and rejected on the other would break every PR, and the parser accepting
// unknown keys is exactly the property that has to stay true.

func liveValidatedPipelineBody(t *testing.T, scenarios []types.TestScenario, verdict string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Tested:         []string{"`npm run e2e -- checkout`"},
		TestingSummary: "drove the checkout scenarios against a running app",
		Scenarios:      scenarios,
		Verdict:        verdict,
		TestedHeadSHA:  requiredWorkflowTestHeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	findingsJSON := string(raw)
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findingsJSON},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusRunning},
	}
	rounds := make(map[string][]*db.StepRound, len(stepResults))
	for _, sr := range stepResults {
		rounds[sr.ID] = []*db.StepRound{{Round: 1, Trigger: "initial", DurationMS: 1, FindingsJSON: sr.FindingsJSON}}
	}
	md, _ := steps.BuildPipelineSummary(stepResults, rounds, requiredWorkflowTestHeadSHA)
	if md == "" {
		t.Fatal("BuildPipelineSummary returned empty markdown")
	}
	return md
}

// attestationPayload extracts the live attestation JSON from a rendered body
// the same way the gate does: first marker wins.
func attestationPayload(t *testing.T, body string) map[string]any {
	t.Helper()
	const prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
	const closing = " -->"
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("no attestation in body:\n%s", body)
	}
	start += len(prefix)
	end := strings.Index(body[start:], closing)
	if end < 0 {
		t.Fatalf("unterminated attestation in body:\n%s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body[start:start+end]), &payload); err != nil {
		t.Fatalf("attestation is not JSON: %v", err)
	}
	return payload
}

func TestRequireActionAcceptsAttestationCarryingLiveValidation(t *testing.T) {
	body := liveValidatedPipelineBody(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
		{Name: "declined payment shows the retry copy", Result: types.ScenarioResultUntested, Reason: "no card sandbox credential"},
	}, types.TestVerdictGo)

	payload := attestationPayload(t, body)
	live, ok := payload["live_validation"].(map[string]any)
	if !ok {
		t.Fatalf("attestation carries no live_validation object: %v", payload)
	}
	if live["verdict"] != types.TestVerdictGo {
		t.Errorf("live_validation.verdict = %v, want %q", live["verdict"], types.TestVerdictGo)
	}
	if live["live"] != float64(1) || live["total"] != float64(2) {
		t.Errorf("live_validation coverage = %v of %v, want 1 of 2", live["live"], live["total"])
	}
	if _, present := live["source"]; present {
		t.Errorf("live_validation must not carry a producer source: %v", live)
	}

	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1568"})
	if result.conclusion != "success" {
		t.Fatalf("gate rejected a body carrying live_validation: %s", result.output)
	}
}

// A pull request whose test step recorded no verdict - every run from before
// the contract - must keep passing the gate, and must not claim a validation
// state nobody established.
func TestRequireActionAcceptsAttestationWithoutLiveValidation(t *testing.T) {
	body := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	if _, present := attestationPayload(t, body)["live_validation"]; present {
		t.Fatal("a run with no recorded verdict must omit live_validation entirely")
	}
	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1568"})
	if result.conclusion != "success" {
		t.Fatalf("gate rejected a pre-contract body: %s", result.output)
	}
}

// The gate certifies step lifecycle, not product quality: a no-go verdict is
// the Test step's own business (it parks the step, so the step never reaches
// "completed" while it stands). The action must not start second-guessing a
// verdict that reaches it.
func TestRequireActionDoesNotAdjudicateTheVerdict(t *testing.T) {
	body := liveValidatedPipelineBody(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultFail, Live: true},
	}, types.TestVerdictNoGo)
	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1568"})
	if result.conclusion != "success" {
		t.Fatalf("gate must certify step lifecycle only, got: %s", result.output)
	}
}
