package db

import (
	"testing"
)

func TestFixProgressRescueStoreBindsSource(t *testing.T) {
	d, repo, run := openSessionTestDB(t)
	p, err := d.BeginWorkRescue(run, "review", "selection", "parent", "/retained/worktree")
	if err != nil {
		t.Fatal(err)
	}
	p.State = "saved"
	p.Ref = "refs/no-mistakes/rescue/" + run.ID + "/review/" + p.StopID
	p.SHA = "snapshot"
	p.IndexSHA = "index"
	if err = d.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	got, err := d.LatestWorkRescue(run.ID)
	if err != nil || got == nil || got.SHA != "snapshot" || got.RepoID != repo.ID || got.Branch != run.Branch {
		t.Fatalf("stored rescue: %+v %v", got, err)
	}
	changed := *got
	changed.ParentHead = "different"
	if err = d.SaveWorkRescue(&changed); err == nil {
		t.Fatal("changed rescue binding accepted")
	}
	if got.State != "saved" {
		t.Fatal("saved work not represented")
	}
}

func TestRescueStoreRetentionCannotBeHiddenByALaterSnapshot(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	p, err := d.BeginWorkRescue(run, "review", "", "parent", "/retained/worktree")
	if err != nil {
		t.Fatal(err)
	}
	p.State = "retained"
	if err := d.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	later, err := d.BeginWorkRescue(run, "lint", "", "parent", "/retained/worktree")
	if err != nil {
		t.Fatal(err)
	}
	later.State = "saved"
	if err := d.SaveWorkRescue(later); err != nil {
		t.Fatal(err)
	}
	got, err := d.LatestWorkRescue(run.ID)
	if err != nil || got == nil || got.StopID != p.StopID {
		t.Fatalf("retention hidden by later snapshot: %+v %v", got, err)
	}
}

func TestRescueStoreMissingStateIsUnreadable(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if _, err := d.sql.Exec(`INSERT INTO run_work_rescues(stop_id,run_id,payload) VALUES(?,?,?)`, "broken", run.ID, `{"version":1}`); err != nil {
		t.Fatal(err)
	}
	if got, err := d.LatestWorkRescue(run.ID); err == nil {
		t.Fatalf("missing state authorized a clean result: %+v", got)
	}
}
