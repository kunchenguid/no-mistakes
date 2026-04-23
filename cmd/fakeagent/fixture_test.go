package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFixtureFileErrorsWhenConfiguredAgentDirectoryMissing(t *testing.T) {
	t.Setenv("FAKEAGENT_FIXTURE", t.TempDir())

	dir := fixtureDir("opencode")
	if dir != filepath.Join(os.Getenv("FAKEAGENT_FIXTURE"), "opencode") {
		t.Fatalf("dir = %q, want joined agent fixture path", dir)
	}

	data, err := readFixtureFile(dir, "structured", ".jsonl")
	if err == nil {
		t.Fatal("expected error for missing configured agent fixture directory")
	}
	if data != nil {
		t.Fatalf("data = %q, want nil", data)
	}
	if !strings.Contains(err.Error(), "missing fixture") {
		t.Fatalf("error = %q, want missing fixture", err)
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("error = %q, want agent path detail", err)
	}
}

func TestReadFixtureFileErrorsWhenConfiguredFixtureMissing(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	data, err := readFixtureFile(dir, "structured", ".jsonl")
	if err == nil {
		t.Fatal("expected error for missing configured fixture")
	}
	if data != nil {
		t.Fatalf("data = %q, want nil", data)
	}
	if !strings.Contains(err.Error(), "missing fixture") {
		t.Fatalf("error = %q, want missing fixture", err)
	}
	if !strings.Contains(err.Error(), "structured") {
		t.Fatalf("error = %q, want structured path detail", err)
	}
}
