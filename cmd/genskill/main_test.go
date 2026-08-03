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

// A reference file the generator stopped producing is drift too: it stays
// committed and readable while `no-mistakes init` no longer ships it, so agents
// read stale guidance nothing regenerates. The check must fail on it, and
// regenerating must be the fix.
func TestGeneratedDirRejectsAndRemovesOrphanFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-mistakes")
	if err := writeDir(dir); err != nil {
		t.Fatalf("writeDir: %v", err)
	}
	orphan := filepath.Join(dir, "retired-reference.md")
	if err := os.WriteFile(orphan, []byte("stale guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := checkDir(dir)
	if err == nil {
		t.Fatal("checkDir accepted a file the generator does not produce")
	}
	if !strings.Contains(err.Error(), "retired-reference.md") {
		t.Errorf("error = %q, want it to name the orphan file", err)
	}

	if err := writeDir(dir); err != nil {
		t.Fatalf("writeDir: %v", err)
	}
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Errorf("orphan survived regeneration: %v", statErr)
	}
	for _, f := range skill.Files() {
		if _, statErr := os.Stat(filepath.Join(dir, f.Name)); statErr != nil {
			t.Errorf("pruning removed a generated file: %v", statErr)
		}
	}
	if err := checkDir(dir); err != nil {
		t.Errorf("checkDir after regeneration: %v", err)
	}
}

// The generator only ever writes plain files, so an entry of any other shape is
// something it does not own. dir is a cwd-relative path, so pruning must refuse
// there instead of recursively deleting a tree it cannot account for.
func TestGeneratedDirRefusesEntriesItCannotOwnInsteadOfDeletingThem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-mistakes")
	if err := writeDir(dir); err != nil {
		t.Fatalf("writeDir: %v", err)
	}
	nested := filepath.Join(dir, "vendored")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(nested, "keep.md")
	if err := os.WriteFile(kept, []byte("not ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, err := range map[string]error{"checkDir": checkDir(dir), "writeDir": writeDir(dir)} {
		if err == nil {
			t.Errorf("%s accepted a directory the generator does not own", name)
			continue
		}
		if !strings.Contains(err.Error(), "vendored") {
			t.Errorf("%s error = %q, want it to name the entry", name, err)
		}
	}
	if _, statErr := os.Stat(kept); statErr != nil {
		t.Errorf("refusal still deleted the unowned tree: %v", statErr)
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
