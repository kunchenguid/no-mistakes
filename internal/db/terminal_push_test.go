package db

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestUpdateRunStatusReleasesOnlyTerminalPushOwners(t *testing.T) {
	for _, status := range []types.RunStatus{
		types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunChecksPassed,
		types.RunCIMonitorInterrupted, types.RunPending, types.RunRunning, "unknown",
	} {
		t.Run(string(status), func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/project", "https://example.com/project.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunPushActive(run.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, status); err != nil {
				t.Fatal(err)
			}
			got, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.PushActive == status.Terminal() {
				t.Fatalf("status %s: push_active = %t", status, got.PushActive)
			}
		})
	}
}

func TestRecoverStaleRunsReleasesExistingTerminalPushOwners(t *testing.T) {
	for _, status := range []types.RunStatus{
		types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunChecksPassed,
		types.RunCIMonitorInterrupted, types.RunPending, types.RunRunning, "unknown",
	} {
		t.Run(string(status), func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/project", "https://example.com/project.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunPublication(run.ID, PushBinding{
				HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature",
			}); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, status); err != nil {
				t.Fatal(err)
			}
			// Seed the durable marker left behind by older terminalization paths.
			if err := d.SetRunPushActive(run.ID, true); err != nil {
				t.Fatal(err)
			}
			before, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			preserved := map[string]struct{}{}
			if !status.Terminal() {
				preserved[run.ID] = struct{}{}
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := d.RecoverStaleRunsExcept("daemon restarted", preserved); err != nil {
					t.Fatal(err)
				}
				got, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := *before
				want.PushActive = !status.Terminal()
				if !reflect.DeepEqual(got, &want) {
					t.Fatalf("recovery %d: got %+v, want %+v", attempt, got, &want)
				}
			}
		})
	}
}
