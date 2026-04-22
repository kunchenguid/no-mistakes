package main

import (
	"strings"
	"testing"
)

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
