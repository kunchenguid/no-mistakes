package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoveredGateUsesPersistedCertifyCandidateNotInitialRunHead(t *testing.T) {
	database, p, run, repo := setupTest(t)
	initialHead := "1111111111111111111111111111111111111111"
	candidateHead := "2222222222222222222222222222222222222222"
	run.HeadSHA = initialHead
	if err := database.UpdateRunHeadSHA(run.ID, initialHead); err != nil {
		t.Fatal(err)
	}

	stepNames := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepCertify,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	var certifyResultID string
	for i, name := range stepNames {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case i < 6:
			if err := database.StartStep(result.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteStep(result.ID, 0, 1, "step.log"); err != nil {
				t.Fatal(err)
			}
		case name == types.StepCertify:
			certifyResultID = result.ID
			findings := `{"findings":[{"id":"cert-1","severity":"warning","description":"operator decision","action":"ask-user"}],"summary":"blocked"}`
			if err := database.StartStep(result.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(result.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertCertifyStepRound(result.ID, 1, "initial", &findings, nil, candidateHead, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(result.ID, types.StepStatusAwaitingApproval, 1); err != nil {
				t.Fatal(err)
			}
		default:
			// The rows after a parked gate remain pending for recovery.
		}
	}
	if certifyResultID == "" {
		t.Fatal("did not create Certify result")
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	steps := make([]Step, 0, len(stepNames))
	for _, name := range stepNames {
		steps = append(steps, &mockStep{name: name, outcome: &StepOutcome{}})
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	gate, err := exec.recoveredGate(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gate.step.Name() != types.StepCertify {
		t.Fatalf("recovered gate step = %s, want certify", gate.step.Name())
	}
	if gate.certifiedHeadSHA != candidateHead {
		t.Fatalf("recovered certification candidate = %q, want %q (initial run head %q must not be used)", gate.certifiedHeadSHA, candidateHead, initialHead)
	}

	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original := testReviewFleetConfig(bin)
	originalSettings, err := reviewFleetSettingsFromConfig(original)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := reviewFleetFingerprint(original, originalSettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewFleetMode(run.ID, true, &fingerprint); err != nil {
		t.Fatal(err)
	}
	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := testReviewFleetConfig(bin)
	changedConfig.ReviewFleet.Certifier.ReasoningEffort = "high"
	resumer := NewExecutor(database, p, changedConfig, nil, steps, nil)
	if err := resumer.Resume(context.Background(), recovered, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "contract changed") {
		t.Fatalf("Resume accepted changed fleet contract: %v", err)
	}
}

func TestCompatibleRecoveryPlanAcceptsLegacyNineStepRun(t *testing.T) {
	current := []Step{
		&mockStep{name: types.StepIntent},
		&mockStep{name: types.StepRebase},
		&mockStep{name: types.StepReview},
		&mockStep{name: types.StepTest},
		&mockStep{name: types.StepDocument},
		&mockStep{name: types.StepLint},
		&mockStep{name: types.StepCertify},
		&mockStep{name: types.StepPush},
		&mockStep{name: types.StepPR},
		&mockStep{name: types.StepCI},
	}
	legacyNames := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	results := make([]*db.StepResult, 0, len(legacyNames))
	for _, name := range legacyNames {
		results = append(results, &db.StepResult{StepName: name})
	}
	plan, err := compatibleRecoveryPlan(results, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != len(legacyNames) {
		t.Fatalf("legacy recovery plan length = %d, want %d", len(plan), len(legacyNames))
	}
	for i, step := range plan {
		if step.Name() != legacyNames[i] {
			t.Errorf("legacy recovery step %d = %s, want %s", i, step.Name(), legacyNames[i])
		}
	}
}
