package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestRunTranscriptHomesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	homes := &runenv.TranscriptHomes{ClaudeConfigDir: filepath.Join(t.TempDir(), "claude account"), CodexHome: filepath.Join(t.TempDir(), "codex account")}
	run, err := d.InsertRunWithTranscriptHomes(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, nil, RunPRText{}, homes)
	if err != nil {
		t.Fatal(err)
	}
	invalidHomes := &runenv.TranscriptHomes{ClaudeConfigDir: "", CodexHome: "~/account"}
	invalidRun, err := d.InsertRunWithTranscriptHomes(repo.ID, "invalid-home", "head", "base", nil, "", "", "", "", false, nil, RunPRText{}, invalidHomes)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil || got == nil || got.TranscriptHomes == nil || *got.TranscriptHomes != *homes {
		t.Fatalf("reopened homes = %+v, %v", got, err)
	}
	invalidGot, err := d.GetRun(invalidRun.ID)
	if err != nil || invalidGot == nil || invalidGot.TranscriptHomes == nil || *invalidGot.TranscriptHomes != *invalidHomes {
		t.Fatalf("reopened invalid homes = %+v, %v", invalidGot, err)
	}
	// Read-only commands must still work before the new migration is applied.
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN transcript_homes`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err = readonly.GetRun(run.ID)
	readonly.Close()
	if err != nil || got == nil || got.TranscriptHomes != nil {
		t.Fatalf("pre-migration read = %+v, %v", got, err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	got, err = migrated.GetRun(run.ID)
	if err != nil || got == nil || got.TranscriptHomes != nil {
		t.Fatalf("historical run gained an account selection: %+v, %v", got, err)
	}
}
