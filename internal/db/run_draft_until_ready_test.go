package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A database created before the draft-PR policy existed must migrate cleanly
// and read back as "not a draft run", never as an unset value that could make
// an old run open a draft PR nobody asked for.
func TestOpenMigratesRunDraftUntilReadyDefaultingToOff(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO repos VALUES ('repo-1', '/work/repo', 'https://example.com/repo.git', 'main', 1);
		INSERT INTO runs VALUES ('run-1', 'repo-1', 'feature', 'head', 'base', 'completed', NULL, NULL, 1, 1);
	`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	run, err := d.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("legacy run disappeared after migration")
	}
	if run.DraftUntilReady {
		t.Fatal("a legacy run must not read back as draft-until-ready")
	}

	if err := d.SetRunDraftUntilReady("run-1", true); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !run.DraftUntilReady {
		t.Fatal("draft_until_ready did not persist on a migrated row")
	}
}

// The draft-until-ready policy carries forward to later runs on the same
// branch, so run creation needs the branch's most recent run regardless of its
// status - including a failed or cancelled one, which is exactly the case a
// rerun follows.
func TestLatestRunForBranchReturnsTheMostRecentRunOnThatBranch(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}

	if run, err := d.LatestRunForBranch("repo-1", "feature"); err != nil || run != nil {
		t.Fatalf("LatestRunForBranch on an unseen branch = %v, %v; want nil, nil", run, err)
	}

	first, err := d.InsertRun("repo-1", "feature", "head-1", "base-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunDraftUntilReady(first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(first.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	// A run on another branch must never be mistaken for this branch's latest.
	if _, err := d.InsertRun("repo-1", "other", "head-x", "base-x"); err != nil {
		t.Fatal(err)
	}

	latest, err := d.LatestRunForBranch("repo-1", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ID != first.ID {
		t.Fatalf("LatestRunForBranch = %v, want the failed run %s", latest, first.ID)
	}
	if !latest.DraftUntilReady {
		t.Fatal("latest run lost its draft-until-ready policy")
	}

	second, err := d.InsertRun("repo-1", "feature", "head-2", "base-2")
	if err != nil {
		t.Fatal(err)
	}
	latest, err = d.LatestRunForBranch("repo-1", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ID != second.ID {
		t.Fatalf("LatestRunForBranch = %v, want the newest run %s", latest, second.ID)
	}
}
