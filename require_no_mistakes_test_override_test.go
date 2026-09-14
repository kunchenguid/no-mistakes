package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const noMistakesSignature = "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"

func attestationBody(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "## Pipeline\n\n" + noMistakesSignature + "\n\n<!-- no-mistakes-pipeline-attestation:v1 " + string(raw) + " -->\n"
}

func completedSteps(overrideReason string) []map[string]any {
	test := map[string]any{"step": "test", "status": "completed"}
	if overrideReason != "" {
		test["override_reason"] = overrideReason
	}
	return []map[string]any{
		{"step": "review", "status": "completed"},
		test,
		{"step": "document", "status": "completed"},
	}
}

func TestRequireActionRefusesApprovedOverFailureTestStepWithoutOptIn(t *testing.T) {
	reason := "configured test command failed with exit code 7"
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &reason},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	body, _ := steps.BuildPipelineSummary(stepResults, nil, requiredWorkflowTestHeadSHA)
	if !strings.Contains(body, `"override_reason"`) {
		t.Fatalf("producer omitted override_reason:\n%s", body)
	}

	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1701"})
	if result.conclusion != "failure" {
		t.Fatalf("gate accepted an approved-over-failure test step without opt-in: %s", result.output)
	}
	if !strings.Contains(result.output, "approved over a failing configured test command") {
		t.Fatalf("failure did not name the override: %s", result.output)
	}
}

func TestRequireActionAcceptsApprovedOverFailureWhenOptedInWithReason(t *testing.T) {
	body := attestationBody(t, map[string]any{
		"head_sha":                    requiredWorkflowTestHeadSHA,
		"steps":                       completedSteps("configured test command failed with exit code 7"),
		"allow_test_command_override": "legacy suite is red on purpose",
	})
	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1702"})
	if result.conclusion != "success" {
		t.Fatalf("gate rejected an opted-in override: %s", result.output)
	}
}

func TestRequireActionAcceptsOldAttestationWithoutOverrideReason(t *testing.T) {
	body := pipelineSummaryWithStatuses(t, types.StepStatusCompleted, types.StepStatusCompleted, types.StepStatusCompleted)
	if _, present := attestationPayload(t, body)["allow_test_command_override"]; present {
		t.Fatal("pre-field attestation must omit allow_test_command_override")
	}
	payload := attestationPayload(t, body)
	for _, item := range payload["steps"].([]any) {
		step, _ := item.(map[string]any)
		if _, present := step["override_reason"]; present {
			t.Fatalf("pre-field test step carried override_reason: %v", item)
		}
	}

	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1703"})
	if result.conclusion != "success" {
		t.Fatalf("gate rejected a pre-field completed attestation: %s", result.output)
	}
}

func TestRequireActionWhitespaceOptInIsNotAnOptIn(t *testing.T) {
	body := attestationBody(t, map[string]any{
		"head_sha":                    requiredWorkflowTestHeadSHA,
		"steps":                       completedSteps("configured test command failed with exit code 7"),
		"allow_test_command_override": "   ",
	})
	result := runRequireAction(t, actionRun{body: body, headSHA: requiredWorkflowTestHeadSHA, number: "1704"})
	if result.conclusion != "failure" {
		t.Fatalf("whitespace opt-in must not pass: %s", result.output)
	}
}
