package verificationplan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureAcceptsExactSizeLimit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	original := bytes.Repeat([]byte("a"), 65536)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Capture(filepath.Join(root, "inputs"), source, "repo", "branch", "head")
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.Read()
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("accepted snapshot changed: %v", err)
	}
}

func TestCaptureRejectsOneByteOverSizeLimit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	original := bytes.Repeat([]byte("a"), 65537)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(root, "inputs")
	plan, err := Capture(inputs, source, "repo", "branch", "head")
	if err == nil || !strings.Contains(err.Error(), "64 KiB (65,536 bytes)") || plan != nil {
		t.Fatalf("oversized capture: plan=%v error=%v", plan, err)
	}
	if _, err := os.Stat(inputs); !os.IsNotExist(err) {
		t.Fatalf("rejected input created capture storage: %v", err)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("rejected source changed: %v", err)
	}
}

func TestSnapshotSurvivesSourceRemovalAndRejectsTampering(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	original := []byte("  Verify café\r\nExpected: exact bytes.\n\n")
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Capture(filepath.Join(root, "inputs"), source, "repo", "branch", "head")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(filepath.Join(root, "inputs"), plan.ID, "repo", "branch", "head")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolved.Read()
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("snapshot changed: %q %v", got, err)
	}
	if _, err := Resolve(filepath.Join(root, "inputs"), plan.ID, "repo", "other", "head"); err == nil {
		t.Fatal("capture rebound across branches")
	}
	if err := os.Chmod(plan.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Read(); err == nil {
		t.Fatal("tampered evidence accepted")
	}
}
