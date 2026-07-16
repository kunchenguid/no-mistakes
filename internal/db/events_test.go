package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLifecycleEventsPreserveFailureAcrossCancellationProjection(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.UpdateRunErrorStatus(run.ID, "codex authorization required", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatus(run.ID, types.RunCancelReasonSuperseded, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	events, err := d.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure, sawCancellation bool
	for _, event := range events {
		if event.EventType == "run_failure" && event.Error == "codex authorization required" {
			sawFailure = true
		}
		if event.Status == string(types.RunCancelled) {
			sawCancellation = true
		}
	}
	if !sawFailure || !sawCancellation {
		t.Fatalf("events lost transition history: %+v", events)
	}
}

func TestRouteDecisionStoresPromptFingerprintWithoutPrompt(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	hash, size := PromptEvidence("secret prompt")
	if err := d.InsertRouteDecision(RouteDecision{RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex", PolicyVersion: "v1", Phase: "review", Reason: "test", PromptSHA256: hash, PromptBytes: size, PromptTransport: "stdin"}); err != nil {
		t.Fatal(err)
	}
	decisions, err := d.RouteDecisions(run.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, err = %v", decisions, err)
	}
	if decisions[0].PromptSHA256 != hash || decisions[0].PromptBytes != size || decisions[0].PromptTransport != "stdin" {
		t.Fatalf("prompt evidence = %+v", decisions[0])
	}
}

func TestProvisioningProjectionIsRestartReadable(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.SetRunProvisioning(run.ID, "checkout", 42, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetProvisioningRuns()
	if err != nil || len(got) != 1 {
		t.Fatalf("provisioning runs = %+v, err = %v", got, err)
	}
	if got[0].ProvisioningPhase != "checkout" || got[0].ProvisioningProgress != 42 {
		t.Fatalf("projection = %+v", got[0])
	}
	if err := d.CompleteRunProvisioning(run.ID); err != nil {
		t.Fatal(err)
	}
	gotRun, err := d.GetRun(run.ID)
	if err != nil || gotRun.Status != types.RunPending || gotRun.ProvisioningProgress != 100 {
		t.Fatalf("completed projection = %+v, err = %v", gotRun, err)
	}
}
