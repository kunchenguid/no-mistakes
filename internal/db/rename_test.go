package db

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func preparePushTargetMigration(t *testing.T) (*DB, PushTargetMigration) {
	t.Helper()
	d := openTestDB(t)
	return d, preparePushTargetMigrationInDB(t, d)
}

func preparePushTargetMigrationInDB(t *testing.T, d *DB) PushTargetMigration {
	t.Helper()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/org/current", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "pushed", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed", TargetKind: "upstream", TargetFingerprint: "previous", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatus(run.ID, "validation failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	return PushTargetMigration{RunID: run.ID, RepoID: repo.ID, Branch: "feature", Status: types.RunFailed, HeadSHA: "pushed", TargetKind: "upstream", PreviousFingerprint: "previous", CurrentFingerprint: "current", Ref: "refs/heads/feature", CurrentUpstreamURL: repo.UpstreamURL, Generation: 1}
}

func TestMigrateRunPushTargetAtomicallyRecordsProvenance(t *testing.T) {
	d, snapshot := preparePushTargetMigration(t)
	before, err := d.GetRun(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER interrupt_migration BEFORE INSERT ON push_target_migrations BEGIN SELECT RAISE(ABORT, 'interrupted'); END`); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.MigrateRunPushTarget(snapshot); err == nil || changed {
		t.Fatalf("interrupted provenance write = %t, %v", changed, err)
	}
	after, err := d.GetRun(snapshot.RunID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("interruption changed run: before=%+v after=%+v, %v", before, after, err)
	}
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM push_target_migrations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("interruption left provenance: %d, %v", count, err)
	}
	if _, err := d.sql.Exec(`DROP TRIGGER interrupt_migration`); err != nil {
		t.Fatal(err)
	}
	for _, wantChanged := range []bool{true, false} {
		if changed, err := d.MigrateRunPushTarget(snapshot); err != nil || changed != wantChanged {
			t.Fatalf("migration changed=%t, %v; want %t", changed, err, wantChanged)
		}
		witnesses, err := d.GetPushTargetRenameWitnesses(snapshot.RepoID, snapshot.Branch, "previous")
		if err != nil || len(witnesses) != 1 || witnesses[0].ID != snapshot.RunID {
			t.Fatalf("verified witness = %+v, %v", witnesses, err)
		}
	}
	conflicting := snapshot
	conflicting.PreviousFingerprint = "another"
	if changed, err := d.MigrateRunPushTarget(conflicting); err == nil || changed {
		t.Fatalf("conflicting provenance accepted: %t, %v", changed, err)
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM push_target_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repetition changed provenance count: %d, %v", count, err)
	}
}

func TestPushTargetRenameWitnessRequiresExactRecordedBinding(t *testing.T) {
	for _, change := range []string{
		"head_sha = 'changed'", "last_pushed_sha = 'changed'", "push_generation = 2", "branch = 'other'",
		"status = 'running'", "status = 'completed'", "push_target_fingerprint = 'other'", "push_target_kind = 'fork'",
		"push_ref = 'refs/heads/other'", "push_active = 1", "submitted_head_sha = NULL",
	} {
		t.Run(change, func(t *testing.T) {
			d, snapshot := preparePushTargetMigration(t)
			if _, err := d.MigrateRunPushTarget(snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`UPDATE runs SET `+change+` WHERE id = ?`, snapshot.RunID); err != nil {
				t.Fatal(err)
			}
			witnesses, err := d.GetPushTargetRenameWitnesses(snapshot.RepoID, snapshot.Branch, "previous")
			if err != nil || len(witnesses) != 0 {
				t.Fatalf("changed binding retained rename authority: %+v, %v", witnesses, err)
			}
		})
	}
}

func TestMigrateRunPushTargetSequentialRenames(t *testing.T) {
	d, first := preparePushTargetMigration(t)
	if _, err := d.MigrateRunPushTarget(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.PreviousFingerprint, second.CurrentFingerprint = first.CurrentFingerprint, "next"
	before, err := d.GetRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantChanged := range []bool{true, false} {
		if changed, err := d.MigrateRunPushTarget(second); err != nil || changed != wantChanged {
			t.Fatalf("second rename = %t, %v; want changed=%t", changed, err, wantChanged)
		}
	}
	after, err := d.GetRun(first.RunID)
	before.PushTargetFingerprint = &second.CurrentFingerprint
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("second rename changed validation history: before=%+v after=%+v, %v", before, after, err)
	}
	for _, previous := range []string{first.PreviousFingerprint, second.PreviousFingerprint} {
		witnesses, err := d.GetPushTargetRenameWitnesses(first.RepoID, first.Branch, previous)
		if err != nil || len(witnesses) != 1 || witnesses[0].ID != first.RunID {
			t.Fatalf("chain from %s lost its witness: %+v, %v", previous, witnesses, err)
		}
	}
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM push_target_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("sequential rename provenance count: %d, %v", count, err)
	}
}

func TestPushTargetRenameChainRequiresEveryEdge(t *testing.T) {
	for _, change := range []string{
		"head_sha = 'changed'", "status = 'completed'", "push_generation = 2", "branch = 'other'",
		"current_fingerprint = 'unrelated'", "target_kind = 'fork'", "push_ref = 'refs/heads/other'",
	} {
		t.Run(change, func(t *testing.T) {
			d, first := preparePushTargetMigration(t)
			if _, err := d.MigrateRunPushTarget(first); err != nil {
				t.Fatal(err)
			}
			second := first
			second.PreviousFingerprint, second.CurrentFingerprint = first.CurrentFingerprint, "next"
			if _, err := d.MigrateRunPushTarget(second); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`UPDATE push_target_migrations SET `+change+` WHERE previous_fingerprint = ?`, first.PreviousFingerprint); err != nil {
				t.Fatal(err)
			}
			for previous, want := range map[string]int{first.PreviousFingerprint: 0, second.PreviousFingerprint: 1} {
				witnesses, err := d.GetPushTargetRenameWitnesses(first.RepoID, first.Branch, previous)
				if err != nil || len(witnesses) != want {
					t.Fatalf("witnesses from %s through changed edge = %+v, %v; want %d", previous, witnesses, err, want)
				}
			}
		})
	}
}

func TestOpenMigratesSingleRenameProvenanceWithoutLosingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	first := preparePushTargetMigrationInDB(t, d)
	// This is the persisted SQLite contract from the first rename schema,
	// including its one-record-per-run constraint, not implementation text.
	if _, err := d.sql.Exec(`DROP TABLE push_target_migrations;
		CREATE TABLE push_target_migrations (
			run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
			repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
			branch TEXT NOT NULL, status TEXT NOT NULL, head_sha TEXT NOT NULL,
			target_kind TEXT NOT NULL, previous_fingerprint TEXT NOT NULL,
			current_fingerprint TEXT NOT NULL, push_ref TEXT NOT NULL, push_generation INTEGER NOT NULL
		);
		INSERT INTO push_target_migrations
			SELECT id, repo_id, branch, status, head_sha, push_target_kind,
			push_target_fingerprint, 'current', push_ref, push_generation FROM runs;
		UPDATE runs SET push_target_fingerprint = 'current';`); err != nil {
		t.Fatal(err)
	}
	before, err := d.GetRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		d, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := d.MigrateRunPushTarget(first); err != nil || changed {
			t.Fatalf("legacy rename did not remain idempotent after reopen: %t, %v", changed, err)
		}
		after, err := d.GetRun(first.RunID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("schema migration changed history: before=%+v after=%+v, %v", before, after, err)
		}
	}
	second := first
	second.PreviousFingerprint, second.CurrentFingerprint = first.CurrentFingerprint, "next"
	if changed, err := d.MigrateRunPushTarget(second); err != nil || !changed {
		t.Fatalf("second rename after schema migration: %t, %v", changed, err)
	}
	witnesses, err := d.GetPushTargetRenameWitnesses(first.RepoID, first.Branch, first.PreviousFingerprint)
	if err != nil || len(witnesses) != 1 || witnesses[0].ID != first.RunID {
		t.Fatalf("legacy rename lost continuity: %+v, %v", witnesses, err)
	}
}
