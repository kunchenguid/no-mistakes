package db

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestHandoffStatusIsTerminalAndClearsPushActivity(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.test/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("UPDATE runs SET push_active = 1 WHERE id = ?", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunHandedOff); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunHandedOff || !got.Status.Terminal() || got.PushActive {
		t.Fatalf("handoff state = %+v", got)
	}
	// Later PR observations must not turn an explicit handoff into a merge claim.
	if err := d.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunHandedOff {
		t.Fatalf("late PR observation changed handoff to %s", got.Status)
	}
}
