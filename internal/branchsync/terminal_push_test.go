package branchsync

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalRunWithUnfinishedPushBindingRecoversAfterRestart(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunCIMonitorInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetRunPushActive(f.run.ID, true); err != nil {
		t.Fatal(err)
	}
	before := f.service.InspectCached(f.ctx)
	if before.State != StatePushInProgress || before.NextAction == nil || before.NextAction.Code != "continue_active_run" {
		t.Fatalf("did not reproduce terminal push ownership: %#v", before)
	}
	if _, err := f.db.RecoverStaleRuns("daemon restarted"); err != nil {
		t.Fatal(err)
	}
	ready := f.service.Refresh(f.ctx)
	if ready.NextAction == nil || ready.NextAction.Code != "sync" {
		t.Fatalf("terminal push ownership still blocks synchronization: %#v", ready)
	}
	result := f.service.Apply(f.ctx)
	if !result.Changed || result.Local.Head != f.pushed {
		t.Fatalf("could not synchronize the published head: %#v", result)
	}
}

func TestRecoveryKeepsPreservedActivePushBindingBlocked(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetRunPushActive(f.run.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecoverStaleRunsExcept("daemon restarted", map[string]struct{}{f.run.ID: {}}); err != nil {
		t.Fatal(err)
	}
	result := f.service.Apply(f.ctx)
	if result.State != StatePushInProgress || result.Changed || result.Local.Head != f.old {
		t.Fatalf("recovery released a preserved active push: %#v", result)
	}
}
