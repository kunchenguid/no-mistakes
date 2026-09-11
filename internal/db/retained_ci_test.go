package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDLOCK31RetainedBindingIsExactAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	old, retained := strings.Repeat("a", 40), strings.Repeat("b", 40)
	run, err := d.InsertRun(repo.ID, "feature", old, old)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, old); err != nil {
		t.Fatal(err)
	}
	binding := RetainedCIRepair{run.ID, ci.ID, repo.ID, "feature", old, old, retained}
	for range 2 {
		if err := d.BindRetainedCIRepair(binding); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"repo", "branch", "step", "retained", "reviewed"} {
		changed := binding
		switch field {
		case "repo":
			changed.RepoID = "other"
		case "branch":
			changed.Branch = "other"
		case "step":
			changed.StepID = "other"
		case "retained":
			changed.RetainedHead = old
		case "reviewed":
			changed.ReviewedHead = retained
		}
		if err := d.BindRetainedCIRepair(changed); err == nil {
			t.Fatalf("replaced %s binding", field)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := d.RetainedCIRepair(run.ID)
	if err != nil || stored == nil || *stored != binding {
		t.Fatalf("binding lost across reopen: %+v %v", stored, err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, retained); err != nil {
		t.Fatal(err)
	}
	if err := d.BindRetainedCIRepair(binding); err == nil {
		t.Fatal("stale review authority accepted")
	}
	if err := d.ClearRetainedCIRepair(run.ID, old); err != nil {
		t.Fatal(err)
	}
	stored, err = d.RetainedCIRepair(run.ID)
	if err != nil || stored == nil {
		t.Fatal("wrong-head cleanup removed binding")
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER refuse_retained_cleanup BEFORE DELETE ON retained_ci_repairs BEGIN SELECT RAISE(FAIL, 'fixture cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPublication(run.ID, PushBinding{HeadSHA: retained}); err == nil {
		t.Fatal("publication ignored binding cleanup failure")
	}
	after, err := d.GetRun(run.ID)
	if err != nil || after.HeadSHA != old || after.PushGeneration != nil {
		t.Fatalf("partial publication escaped transaction: %+v %v", after, err)
	}
}
