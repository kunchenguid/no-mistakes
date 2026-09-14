package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineAttestation_CarriesTestCommandOverride(t *testing.T) {
	t.Parallel()
	reason := "configured test command failed with exit code 7"
	allow := "legacy suite is red on purpose"
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &reason},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}

	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderUnknown, pipelineAttestationPolicy{
		AllowTestCommandOverride: allow,
	})
	attestation := parsePipelineAttestationForTest(t, md)

	var testStep pipelineAttestationStep
	for _, item := range attestation.Steps {
		if item.Step == types.StepTest {
			testStep = item
			break
		}
	}
	if testStep.Status != types.StepStatusCompleted {
		t.Fatalf("test status = %q, want completed", testStep.Status)
	}
	if testStep.OverrideReason != reason {
		t.Fatalf("test override_reason = %q, want %q", testStep.OverrideReason, reason)
	}
	if attestation.AllowTestCommandOverride != allow {
		t.Fatalf("allow_test_command_override = %q, want %q", attestation.AllowTestCommandOverride, allow)
	}

	raw, err := json.Marshal(attestation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"override_reason"`) {
		t.Fatalf("marshaled attestation omitted override_reason: %s", raw)
	}
	if !strings.Contains(string(raw), `"allow_test_command_override"`) {
		t.Fatalf("marshaled attestation omitted allow_test_command_override: %s", raw)
	}
}

func TestBuildPipelineAttestation_OmitsOverrideWhenAbsent(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	raw := buildPipelineAttestation(steps, nil, testPipelineHeadSHA)
	if strings.Contains(raw, "override_reason") {
		t.Fatalf("ordinary completion must omit override_reason: %s", raw)
	}
	if strings.Contains(raw, "allow_test_command_override") {
		t.Fatalf("ordinary completion must omit allow_test_command_override: %s", raw)
	}
}

func TestBuildPipelineSummary_RendersConfiguredTestCommandFailure(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[{"severity":"error","category":"test-command","description":"configured test command failed with exit code 7"}],"summary":""}`
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "configured test command failed with exit code 7") {
		t.Fatalf("PR body omitted the configured-command failure: %s", md)
	}
	if strings.Contains(md, "baseline tests failed") {
		t.Fatalf("PR body still used baseline wording: %s", md)
	}
}

func TestRebindPipelineAttestation_PreservesTestCommandOverride(t *testing.T) {
	t.Parallel()
	reason := "configured test command failed with exit code 7"
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &reason},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	original := buildPipelineAttestationWithPolicy(steps, nil, testPipelineHeadSHA, pipelineAttestationPolicy{
		AllowTestCommandOverride: "legacy suite is red on purpose",
	})

	rebound, ok := rebindPipelineAttestationHead(original, strings.Repeat("cd", 20))
	if !ok {
		t.Fatal("expected attestation to rebind")
	}
	got := parsePipelineAttestationForTest(t, rebound)
	if got.AllowTestCommandOverride != "legacy suite is red on purpose" {
		t.Fatalf("rebound dropped allow_test_command_override: %+v", got)
	}
	var testReason string
	for _, item := range got.Steps {
		if item.Step == types.StepTest {
			testReason = item.OverrideReason
		}
	}
	if testReason != reason {
		t.Fatalf("rebound dropped test override_reason: %+v", got)
	}
}

func TestRebindPipelineAttestation_ClearsRemovedAllowOptIn(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	original := buildPipelineAttestationWithPolicy(steps, nil, testPipelineHeadSHA, pipelineAttestationPolicy{
		AllowTestCommandOverride: "legacy suite is red on purpose",
	})

	rebound, ok := rebindPipelineAttestationWithSteps(original, strings.Repeat("cd", 20), steps, pipelineAttestationPolicy{})
	if !ok {
		t.Fatal("expected attestation to rebind")
	}
	got := parsePipelineAttestationForTest(t, rebound)
	if got.AllowTestCommandOverride != "" {
		t.Fatalf("rebind retained removed opt-in: %+v", got)
	}
}

func TestRebindPipelineAttestation_OverlaysCurrentAllowOptIn(t *testing.T) {
	t.Parallel()
	reason := "configured test command failed with exit code 7"
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &reason},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	original := buildPipelineAttestation(steps, nil, testPipelineHeadSHA)
	if parsePipelineAttestationForTest(t, original).AllowTestCommandOverride != "" {
		t.Fatal("fixture must start without an opt-in")
	}

	rebound, ok := rebindPipelineAttestationWithSteps(original, strings.Repeat("cd", 20), steps, pipelineAttestationPolicy{
		AllowTestCommandOverride: "legacy suite is red on purpose",
	})
	if !ok {
		t.Fatal("expected attestation to rebind")
	}
	got := parsePipelineAttestationForTest(t, rebound)
	if got.AllowTestCommandOverride != "legacy suite is red on purpose" {
		t.Fatalf("rebind did not overlay current opt-in: %+v", got)
	}
}
