package db

import (
	"path/filepath"
	"testing"
)

func TestRunClaudeConfigDirIsImmutable(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, nil, "/caller/.claude1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET claude_config_dir = NULL WHERE id = ?`, run.ID); err == nil {
		t.Fatal("Claude config dir could be cleared")
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.ClaudeConfigDir == nil || *got.ClaudeConfigDir != "/caller/.claude1" {
		t.Fatalf("read Claude config dir: %+v %v", got, err)
	}
	legacy, err := d.InsertRun(repo.ID, "legacy", "h", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET claude_config_dir = '/later' WHERE id = ?`, legacy.ID); err == nil {
		t.Fatal("unset run retroactively bound to a Claude profile")
	}
}

func TestOpenMigratesClaudeConfigDirWithoutBindingHistoricalRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if _, err := d.sql.Exec(`DROP TRIGGER runs_claude_config_dir_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN claude_config_dir`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil || got.ClaudeConfigDir != nil {
		t.Fatalf("migration changed historical run: %+v %v", got, err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET claude_config_dir = '/later' WHERE id = ?`, run.ID); err == nil {
		t.Fatal("migration did not restore the immutability trigger")
	}
}
