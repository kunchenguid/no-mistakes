package db

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunProjectionRollsBackWhenLifecycleEvidenceFails(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	var err error
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_run_failure BEFORE INSERT ON lifecycle_events WHEN NEW.event_type = 'run_failure' BEGIN SELECT RAISE(ABORT, 'injected lifecycle failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatus(run.ID, "boom", types.RunFailed); err == nil {
		t.Fatal("UpdateRunErrorStatus unexpectedly succeeded")
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending || got.Error != nil {
		t.Fatalf("projection committed without lifecycle evidence: status=%s error=%v", got.Status, got.Error)
	}
	events, err := d.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == "run_failure" {
			t.Fatalf("failed lifecycle event persisted: %+v", event)
		}
	}
}

func TestAuthorizationFailureSurvivesLaterCancellationProjection(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.ParkRunForAuthorization(run.ID, "review-fix", "Transport channel closed, when Auth(AuthorizationRequired)"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatus(run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	events, err := d.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundAuth := false
	for _, event := range events {
		if event.EventType == "authorization_required" && strings.Contains(event.Error, "AuthorizationRequired") {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatalf("authorization event was lost: %#v", events)
	}
	current, err := d.GetRun(run.ID)
	if err != nil || current.Status != types.RunCancelled {
		t.Fatalf("current projection = %#v, err=%v", current, err)
	}
	if current.BlockedReason != nil {
		t.Fatalf("terminal projection retained blocked reason: %#v", current.BlockedReason)
	}
}

func TestProvisioningProgressIsPersisted(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.SetRunProvisioning(run.ID, "checkout", 42, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunProvisioning || got.ProvisioningPhase != "checkout" || got.ProvisioningProgress != 42 {
		t.Fatalf("provisioning projection = %#v", got)
	}
}

func TestAuthorizationParkIsRestartPreservedWithoutGateTimer(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.ParkRunForAuthorization(run.ID, "review", "account rotation"); err != nil {
		t.Fatal(err)
	}
	active, err := d.GetActiveRuns()
	if err != nil || len(active) != 1 || active[0].Status != types.RunAwaitingAuth {
		t.Fatalf("active authorization park = %+v, err = %v", active, err)
	}
	if active[0].AwaitingAgentSince != nil {
		t.Fatalf("authorization park incorrectly has gate timer: %v", *active[0].AwaitingAgentSince)
	}
	count, err := d.RecoverStaleRunsExcept("daemon crashed during execution", map[string]struct{}{run.ID: {}})
	if err != nil || count != 0 {
		t.Fatalf("preserved authorization run recovery = %d, err = %v", count, err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.Status != types.RunAwaitingAuth || got.BlockedReason == nil {
		t.Fatalf("authorization park after restart projection = %+v, err = %v", got, err)
	}
}
