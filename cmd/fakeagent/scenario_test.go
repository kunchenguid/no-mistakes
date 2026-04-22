package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunClaudeFailsWhenScenarioEditReplacementMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	scenario := &Scenario{Actions: []Action{{
		Match: "fix it",
		Edits: []Edit{{Path: "note.txt", Old: "missing", New: "after"}},
	}}}

	if code := runClaude([]string{"-p", "fix it"}, scenario); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "before\n" {
		t.Fatalf("file contents = %q, want unchanged", data)
	}
}
