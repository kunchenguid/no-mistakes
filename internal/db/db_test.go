package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenAndClose(t *testing.T) {
	d := openTestDB(t)
	if d == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestOpenReadOnlyRequiresExistingDBAndDoesNotMigrate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := OpenReadOnly(missing); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly missing error = %v, want not-exist", err)
	}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer readonly.Close()
	if _, err := readonly.GetRepos(); err != nil {
		t.Fatalf("read repos: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("read-only open changed DB size from %d to %d", before.Size(), after.Size())
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	d := openTestDB(t)
	// verify tables exist by querying them
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM repos").Scan(&count); err != nil {
		t.Fatalf("repos table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("runs table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM step_results").Scan(&count); err != nil {
		t.Fatalf("step_results table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM recovery_archives").Scan(&count); err != nil {
		t.Fatalf("recovery_archives table missing: %v", err)
	}
	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("repos.fork_url column missing from fresh schema")
	}
	for _, column := range []string{"worktree_dir", "submitted_head_sha", "no_mistakes_version", "no_mistakes_build_sha", "review_approved_head_sha", "last_pushed_sha", "push_target_fingerprint", "push_ref", "last_pushed_at", "push_generation", "push_active", "terminal_head_verified_at", "pr_state", "pr_state_observed_at", "ci_ready_at", "ci_ready_no_ci", "custody_returned_at"} {
		if !hasColumn(t, d, "runs", column) {
			t.Fatalf("runs.%s column missing from fresh schema", column)
		}
	}
	if !hasColumn(t, d, "step_rounds", "reviewed_head_sha") {
		t.Fatal("step_rounds.reviewed_head_sha column missing from fresh schema")
	}
	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid", "ci_fix_attempts"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("step_results.%s column missing from fresh schema", column)
		}
	}
}

func TestOpenMigratesRunSyncProvenanceWithoutBackfillingMutableHead(t *testing.T) {
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
	run, err := d.GetRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.HeadSHA != "mutable-head" {
		t.Fatalf("migrated run = %#v", run)
	}
	if run.SubmittedHeadSHA != nil || run.NoMistakesVersion != nil || run.NoMistakesBuildSHA != nil || run.ReviewApprovedHeadSHA != nil || run.LastPushedSHA != nil || run.PushGeneration != nil || run.PushTargetFingerprint != nil {
		t.Fatalf("legacy provenance, build identity, or review authority was inferred from mutable head: %#v", run)
	}
	if run.CustodyReturnedAt != nil {
		t.Fatalf("legacy run gained a custody-return stamp: %#v", run)
	}
	var archiveCount int
	if err := d.sql.QueryRow("SELECT count(*) FROM recovery_archives").Scan(&archiveCount); err != nil || archiveCount != 0 {
		t.Fatalf("recovery archive migration = count %d, error %v", archiveCount, err)
	}
	// Placement cannot be recovered for a row written before it was recorded,
	// so it reads back as unknown rather than as a guessed directory; callers
	// derive it through worktrees.Layout.RecordedDir.
	if run.WorktreeDir != nil || run.WorktreePath() != "" {
		t.Fatalf("legacy run gained a worktree placement: %#v", run)
	}
}

// A run's worktree placement is durable because the configuration it comes from
// can be edited while the run exists (see internal/worktrees).
func TestSetRunWorktreeDirRecordsPlacementDurably(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if run.WorktreePath() != "" {
		t.Fatalf("new run started with a placement: %q", run.WorktreePath())
	}
	dir := filepath.Join("/work", "repo-runs", run.ID)
	if err := d.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorktreePath() != dir {
		t.Fatalf("recorded placement = %q, want %q", stored.WorktreePath(), dir)
	}
}

// RunWorktreesOutside is how startup recovery finds run worktrees placed
// outside the tree it walks itself, without asking the configuration where they
// might be. It answers from the records only: a run in the default tree is
// found by walking, and a run that never recorded a placement predates the
// column - and therefore predates any placement but the default one.
func TestRunWorktreesOutsideReturnsOnlyRecordedPlacementsElsewhere(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	defaultRoot := filepath.Join("/nm-home", "worktrees")

	inDefaultTree, err := d.InsertRun(repo.ID, "a", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(inDefaultTree.ID, filepath.Join(defaultRoot, repo.ID, inDefaultTree.ID)); err != nil {
		t.Fatal(err)
	}
	unrecorded, err := d.InsertRun(repo.ID, "b", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := d.InsertRun(repo.ID, "c", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	elsewhereDir := filepath.Join("/work", "repo-runs", elsewhere.ID)
	if err := d.SetRunWorktreeDir(elsewhere.ID, elsewhereDir); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(elsewhere.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	got, err := d.RunWorktreesOutside(defaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range got {
		if wt.RunID == inDefaultTree.ID {
			t.Errorf("a placement inside %q was returned: %+v", defaultRoot, wt)
		}
		if wt.RunID == unrecorded.ID {
			t.Errorf("a run with no recorded placement was returned: %+v", wt)
		}
	}
	if len(got) != 1 {
		t.Fatalf("RunWorktreesOutside() = %+v, want only the placement outside %q", got, defaultRoot)
	}
	if got[0].RunID != elsewhere.ID || got[0].RepoID != repo.ID || got[0].Dir != elsewhereDir {
		t.Errorf("RunWorktreesOutside() = %+v, want run %s of %s at %q", got[0], elsewhere.ID, repo.ID, elsewhereDir)
	}
}

// ActiveRunWorktreesOutside is the bounded set the startup process sweep turns
// into path matchers: it must hold the runs that were still executing when the
// daemon started - whose worktrees may already be gone - and not the history of
// every run ever placed outside the default tree.
func TestActiveRunWorktreesOutsideReturnsOnlyRunsStillActive(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	defaultRoot := filepath.Join("/nm-home", "worktrees")

	statuses := map[types.RunStatus]bool{
		types.RunPending:   true,
		types.RunRunning:   true,
		types.RunCompleted: false,
		types.RunFailed:    false,
		types.RunCancelled: false,
	}
	wantActive := make(map[string]bool)
	for status, active := range statuses {
		run, err := d.InsertRun(repo.ID, string(status), "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetRunWorktreeDir(run.ID, filepath.Join("/work", "repo-runs", run.ID)); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, status); err != nil {
			t.Fatal(err)
		}
		if active {
			wantActive[run.ID] = true
		}
	}

	got, err := d.ActiveRunWorktreesOutside(defaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	gotActive := make(map[string]bool, len(got))
	for _, wt := range got {
		gotActive[wt.RunID] = true
	}
	for runID := range wantActive {
		if !gotActive[runID] {
			t.Errorf("active run %s missing from ActiveRunWorktreesOutside()", runID)
		}
	}
	for runID := range gotActive {
		if !wantActive[runID] {
			t.Errorf("terminal run %s returned by ActiveRunWorktreesOutside()", runID)
		}
	}
}

func TestOpenCreatesStepRoundsTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM step_rounds").Scan(&count); err != nil {
		t.Fatalf("step_rounds table missing: %v", err)
	}
}

func TestOpenMigratesExistingStepRoundsColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_rounds (
			id TEXT PRIMARY KEY,
			step_result_id TEXT NOT NULL,
			round INTEGER NOT NULL,
			trigger_type TEXT NOT NULL,
			findings_json TEXT,
			duration_ms INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_rounds table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	rows, err := d.sql.Query(`PRAGMA table_info(step_rounds)`)
	if err != nil {
		t.Fatalf("pragma table_info(step_rounds): %v", err)
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}

	for _, name := range []string{"selected_finding_ids", "selection_source", "fix_summary", "reviewed_head_sha"} {
		if !columns[name] {
			t.Fatalf("expected migrated column %q to exist", name)
		}
	}
}

func TestOpenMigratesReposForkURLColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE repos (
			id TEXT PRIMARY KEY,
			working_path TEXT NOT NULL UNIQUE,
			upstream_url TEXT NOT NULL,
			default_branch TEXT NOT NULL DEFAULT 'main',
			created_at INTEGER NOT NULL
		);
		INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at)
		VALUES ('repo-1', '/work/repo', 'git@github.com:parent/repo.git', 'main', 123);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy repos table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("expected migrated fork_url column")
	}
	repo, err := d.GetRepo("repo-1")
	if err != nil {
		t.Fatalf("get migrated repo: %v", err)
	}
	if repo == nil {
		t.Fatal("expected migrated repo")
	}
	if repo.ForkURL != "" {
		t.Fatalf("fork url = %q, want empty", repo.ForkURL)
	}
	updated, err := d.UpdateRepoForkURL(repo.ID, "git@github.com:fork/repo.git")
	if err != nil {
		t.Fatalf("update migrated fork URL: %v", err)
	}
	if updated.ForkURL != "git@github.com:fork/repo.git" {
		t.Fatalf("fork url after update = %q, want fork URL", updated.ForkURL)
	}
}

func TestOpenMigratesStepActivityColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_results (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			step_name TEXT NOT NULL,
			step_order INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			exit_code INTEGER,
			duration_ms INTEGER,
			log_path TEXT,
			findings_json TEXT,
			error TEXT,
			started_at INTEGER,
			completed_at INTEGER
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_results table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("expected migrated column %q", column)
		}
	}
}

func hasColumn(t *testing.T, d *DB, table, column string) bool {
	t.Helper()
	rows, err := d.sql.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return false
}

func TestOpenWaitsForTransientMigrationLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	locker, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open locker db: %v", err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		d, err := Open(dbPath)
		if err == nil {
			err = d.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned before the migration lock was released")
		}
		t.Fatalf("Open should wait for a transient migration lock, got: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := locker.Exec("COMMIT"); err != nil {
		t.Fatalf("commit exclusive lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not finish after the migration lock was released")
	}
}
