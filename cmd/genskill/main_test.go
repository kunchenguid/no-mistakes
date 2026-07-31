package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

// TestWriteDirThenCheckDir proves the generator renders every skill file and
// that a freshly generated directory passes the drift check `make lint` runs.
func TestWriteDirThenCheckDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-mistakes")
	if err := writeDir(dir); err != nil {
		t.Fatalf("writeDir: %v", err)
	}
	for _, f := range skill.Files() {
		got, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		if string(got) != f.Content {
			t.Errorf("%s content does not match the generator", f.Name)
		}
	}
	if err := checkDir(dir); err != nil {
		t.Errorf("freshly generated skill reported drift: %v", err)
	}
}

// TestCheckDirDetectsReferenceFileDrift is the CI guard: editing or deleting a
// disclosed reference file must fail the check just like a stale SKILL.md.
func TestCheckDirDetectsReferenceFileDrift(t *testing.T) {
	ref := skill.ReadingOutputFile

	t.Run("stale", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "no-mistakes")
		if err := writeDir(dir); err != nil {
			t.Fatalf("writeDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ref), []byte("hand-edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := checkDir(dir)
		if err == nil {
			t.Fatalf("checkDir accepted a hand-edited %s", ref)
		}
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("drift error should name %s, got %v", ref, err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "no-mistakes")
		if err := writeDir(dir); err != nil {
			t.Fatalf("writeDir: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, ref)); err != nil {
			t.Fatal(err)
		}
		if err := checkDir(dir); err == nil {
			t.Fatalf("checkDir accepted a missing %s", ref)
		}
	})
}
