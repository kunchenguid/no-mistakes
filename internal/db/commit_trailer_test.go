package db

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/committrailer"
)

func TestRunCommitTrailersFreshSchemaAndInsertRoundTrip(t *testing.T) {
	d := openTestDB(t)
	if !hasColumn(t, d, "runs", "commit_trailers") {
		t.Fatal("runs.commit_trailers column missing from fresh schema")
	}
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", InsertRunOptions{
		CommitTrailers: trailers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run.CommitTrailers, trailers) {
		t.Fatalf("inserted run trailers = %#v, want %#v", run.CommitTrailers, trailers)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.CommitTrailers, trailers) {
		t.Fatalf("persisted run trailers = %#v, want %#v", got.CommitTrailers, trailers)
	}
}

func TestRunCommitTrailersAbsentInputIsEmpty(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.CommitTrailers) != 0 {
		t.Fatalf("new run with no trailer input has trailers %#v", run.CommitTrailers)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CommitTrailers) != 0 {
		t.Fatalf("persisted run with no trailer input has trailers %#v", got.CommitTrailers)
	}
}

func TestOpenMigratesRunCommitTrailersWithoutBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO repos VALUES ('repo-1', '/work/repo', 'https://example.com/repo.git', 'main', 1);
		INSERT INTO runs VALUES ('run-1', 'repo-1', 'feature', 'mutable-head', 'base', 'completed', NULL, NULL, 1, 1);
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
	if !hasColumn(t, d, "runs", "commit_trailers") {
		t.Fatal("expected migrated commit_trailers column")
	}
	run, err := d.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.CommitTrailers) != 0 {
		t.Fatalf("legacy run inferred commit trailers from mutable state: %#v", run.CommitTrailers)
	}
}
