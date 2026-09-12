package db

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func preparePushTargetMigration(t *testing.T) (*DB, PushTargetMigration) {
	t.Helper()
	d := openTestDB(t)
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
	return d, PushTargetMigration{RunID: run.ID, RepoID: repo.ID, Branch: "feature", Status: types.RunFailed, HeadSHA: "pushed", TargetKind: "upstream", PreviousFingerprint: "previous", CurrentFingerprint: "current", Ref: "refs/heads/feature", CurrentUpstreamURL: repo.UpstreamURL, Generation: 1}
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
		witnesses, err := d.GetPushTargetRenameWitnesses(snapshot.RepoID, snapshot.Branch, "previous", "current")
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
			witnesses, err := d.GetPushTargetRenameWitnesses(snapshot.RepoID, snapshot.Branch, "previous", "current")
			if err != nil || len(witnesses) != 0 {
				t.Fatalf("changed binding retained rename authority: %+v, %v", witnesses, err)
			}
		})
	}
}
