package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestApplyEditsCreatesParentDirectoriesForNewFiles(t *testing.T) {
	dir := t.TempDir()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	if err := applyEdits([]Edit{{Path: filepath.Join("nested", "dir", "note.txt"), New: "hello\n"}}); err != nil {
		t.Fatalf("applyEdits: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "nested", "dir", "note.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("file contents = %q, want %q", data, "hello\n")
	}
}

func TestApplyActionStagesFiles(t *testing.T) {
	dir := t.TempDir()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitCmd("init")

	if err := applyActionInDir(dir, Action{
		Edits: []Edit{{Path: "agent_test.go", New: "package main\n"}},
		Stage: []string{"agent_test.go"},
	}); err != nil {
		t.Fatalf("applyActionInDir: %v", err)
	}

	status := gitCmd("status", "--porcelain")
	if !strings.Contains(status, "A  agent_test.go") {
		t.Fatalf("git status = %q, want staged agent_test.go", status)
	}
}

func TestApplyEditsRejectsPathsOutsideWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	err = applyEdits([]Edit{{Path: filepath.Join("..", filepath.Base(outside)), New: "hello\n"}})
	if err == nil {
		t.Fatal("applyEdits succeeded, want error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists or unexpected error: %v", statErr)
	}
}

func TestApplyEditsRejectsSymlinkPathsOutsideWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")

	linkPath := filepath.Join(dir, "escape")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	err = applyEdits([]Edit{{Path: filepath.Join("escape", "outside.txt"), New: "hello\n"}})
	if err == nil {
		t.Fatal("applyEdits succeeded, want error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists or unexpected error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(linkPath, "outside.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("symlink target file exists or unexpected error: %v", statErr)
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("error = %q, want working directory violation", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote(filepath.Join("escape", "outside.txt"))) {
		t.Fatalf("error = %q, want offending path", err)
	}
}

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
