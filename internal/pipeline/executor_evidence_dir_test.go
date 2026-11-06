package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRunEvidenceDirKeepsThePinnedDirectory(t *testing.T) {
	database, p, run, _ := setupTest(t)
	pinned := filepath.Join(t.TempDir(), "pinned", run.ID)
	if err := database.SetRunEvidenceDir(run.ID, pinned); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, &config.Config{
		Test: config.Test{Evidence: config.Evidence{LocalRoot: filepath.Join(t.TempDir(), "other")}},
	}, nil, nil, nil)
	if got := exec.runEvidenceDir(run.ID); got != pinned {
		t.Fatalf("runEvidenceDir = %q, want the pin %q", got, pinned)
	}
	if err := database.SetRunEvidenceDir(run.ID, filepath.Join(t.TempDir(), "moved", run.ID)); err != nil {
		t.Fatal(err)
	}
	if got := exec.runEvidenceDir(run.ID); got != pinned {
		t.Fatalf("runEvidenceDir after a second pin = %q, want %q", got, pinned)
	}
}
