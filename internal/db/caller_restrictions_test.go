package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
)

func TestCallerRestrictionsAreImmutableAndRunBound(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.test/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, branch := range []string{"one", "two"} {
		path := filepath.Join(t.TempDir(), "caller.txt")
		if err := os.WriteFile(path, []byte("restrictions for "+branch+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := callerrestrictions.Capture(path)
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.InsertRunWithCallerRestrictions(repo.ID, branch, "submitted-"+branch, "base", nil, "", "", "", "", false, nil, RunPRText{}, nil, r)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
		if _, err := d.sql.Exec(`UPDATE runs SET caller_restrictions = NULL WHERE id = ?`, run.ID); err == nil {
			t.Fatal("immutable record removed")
		}
	}
	for i, id := range ids {
		run, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		branch := []string{"one", "two"}[i]
		if run.CallerRestrictions == nil || run.CallerRestrictions.Content != "restrictions for "+branch+"\n" || *run.SubmittedHeadSHA != "submitted-"+branch {
			t.Fatalf("cross-run record mismatch: %+v", run)
		}
	}
	unset, err := d.InsertRun(repo.ID, "unset", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	// An absent record also cannot acquire authority from later evidence.
	first, _ := d.GetRun(ids[0])
	if _, err := d.sql.Exec(`UPDATE runs SET caller_restrictions = ? WHERE id = ?`, first.CallerRestrictions, unset.ID); err == nil {
		t.Fatal("late caller authority accepted")
	}
}
