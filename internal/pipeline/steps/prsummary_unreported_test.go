package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The PR step writes the attestation while it is itself running, and nothing
// rewrites the body when the run ends. These tests pin what that published
// snapshot may say about steps that had not finished yet.

func parsePublishedAttestation(t *testing.T, body string) pipelineAttestation {
	t.Helper()
	start := strings.Index(body, pipelineAttestationCommentPrefix)
	if start < 0 {
		t.Fatalf("body carries no attestation:\n%s", body)
	}
	payloadStart := start + len(pipelineAttestationCommentPrefix)
	end := strings.Index(body[payloadStart:], pipelineAttestationCommentClosingToken)
	if end < 0 {
		t.Fatalf("attestation is not closed:\n%s", body)
	}
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(body[payloadStart:payloadStart+end]), &attestation); err != nil {
		t.Fatalf("attestation payload must be JSON: %v", err)
	}
	return attestation
}

func prWritePointSteps() []*db.StepResult {
	return []*db.StepResult{
		{ID: "intent", StepName: types.StepIntent, Status: types.StepStatusCompleted},
		{ID: "rebase", StepName: types.StepRebase, Status: types.StepStatusCompleted},
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
		{ID: "lint", StepName: types.StepLint, Status: types.StepStatusCompleted},
		{ID: "push", StepName: types.StepPush, Status: types.StepStatusCompleted},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusRunning},
		{ID: "ci", StepName: types.StepCI, Status: types.StepStatusPending},
	}
}

// The published block outlives the run, so it never says a step is still
// running or waiting: that would stay true-looking after the run finished.
func TestPipelineAttestation_NeverPublishesAnInProgressStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []types.StepStatus{
		types.StepStatusPending,
		types.StepStatusRunning,
		types.StepStatusAwaitingApproval,
		types.StepStatusFixing,
		types.StepStatusFixReview,
	} {
		steps := prWritePointSteps()
		steps[len(steps)-1].Status = status
		body, _ := BuildPipelineSummary(steps, nil, testPipelineHeadSHA)
		for _, item := range parsePublishedAttestation(t, body).Steps {
			switch item.Status {
			case types.StepStatusCompleted, types.StepStatusSkipped, types.StepStatusFailed, attestationStatusUnreported:
			default:
				t.Errorf("source status %q: step %q published as %q, want a terminal status or %q", status, item.Step, item.Status, attestationStatusUnreported)
			}
		}
	}
}

// An unreported step must never read as a pass.
func TestPipelineAttestation_UnreportedIsDistinctFromPassed(t *testing.T) {
	t.Parallel()
	body, _ := BuildPipelineSummary(prWritePointSteps(), nil, testPipelineHeadSHA)
	got := map[types.StepName]types.StepStatus{}
	for _, item := range parsePublishedAttestation(t, body).Steps {
		got[item.Step] = item.Status
	}
	if got[types.StepReview] != types.StepStatusCompleted {
		t.Fatalf("review = %q, want completed", got[types.StepReview])
	}
	for _, step := range []types.StepName{types.StepPR, types.StepCI} {
		if got[step] != attestationStatusUnreported {
			t.Errorf("%s = %q, want %q", step, got[step], attestationStatusUnreported)
		}
		if got[step] == types.StepStatusCompleted {
			t.Errorf("%s published as completed though it had not reported", step)
		}
	}
	if attestationStatusUnreported == types.StepStatusCompleted {
		t.Fatal("unreported must be a distinct status from completed")
	}
}

// The live run record is what in-progress views (axi, TUI, status) read; the
// published snapshot must not alter it, so a running run still shows running.
func TestPipelineAttestation_LeavesTheLiveRunningStateAlone(t *testing.T) {
	t.Parallel()
	steps := prWritePointSteps()
	_, _ = BuildPipelineSummary(steps, nil, testPipelineHeadSHA)
	if steps[7].Status != types.StepStatusRunning || steps[8].Status != types.StepStatusPending {
		t.Fatalf("building the attestation changed the live step states: pr=%q ci=%q", steps[7].Status, steps[8].Status)
	}
}

// A run where every step reported publishes exactly what it recorded.
func TestPipelineAttestation_FullyReportedRunIsUnchanged(t *testing.T) {
	t.Parallel()
	steps := prWritePointSteps()
	steps[7].Status = types.StepStatusCompleted
	steps[8].Status = types.StepStatusSkipped
	body, _ := BuildPipelineSummary(steps, nil, testPipelineHeadSHA)
	published := parsePublishedAttestation(t, body).Steps
	for i, item := range published {
		if item.Status != steps[i].Status {
			t.Errorf("step %q published as %q, recorded %q", item.Step, item.Status, steps[i].Status)
		}
	}
}

// The commit the snapshot is bound to stays exactly the run's head, on the
// first write and on every restamp that carries the unreported status forward.
func TestPipelineAttestation_UnreportedKeepsTheNamedCommit(t *testing.T) {
	t.Parallel()
	body, _ := BuildPipelineSummary(prWritePointSteps(), nil, testPipelineHeadSHA)
	if got := parsePublishedAttestation(t, body).HeadSHA; got != testPipelineHeadSHA {
		t.Fatalf("head_sha = %q, want %q", got, testPipelineHeadSHA)
	}
	const newHead = "fedcba9876543210fedcba9876543210fedcba98"
	rebound, ok := rebindPipelineAttestationHead(body, newHead)
	if !ok {
		t.Fatal("restamp did not find the attestation")
	}
	attestation := parsePublishedAttestation(t, rebound)
	if attestation.HeadSHA != newHead {
		t.Fatalf("restamped head_sha = %q, want %q", attestation.HeadSHA, newHead)
	}
	for _, item := range attestation.Steps {
		if item.Step == types.StepPR && item.Status != attestationStatusUnreported {
			t.Fatalf("restamp changed pr status to %q", item.Status)
		}
	}
}
