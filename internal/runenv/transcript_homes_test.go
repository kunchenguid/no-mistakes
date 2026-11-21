package runenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureTranscriptHomesDefaultsToOMPHome(t *testing.T) {
	t.Setenv("OMP_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	got := CaptureTranscriptHomes()
	if got.OMPHome != filepath.Join(home, ".omp") {
		t.Fatalf("OMPHome = %q, want %q", got.OMPHome, filepath.Join(home, ".omp"))
	}
}

func TestCaptureTranscriptHomesUsesOMPHome(t *testing.T) {
	selected := filepath.Join(t.TempDir(), "omp account")
	t.Setenv("OMP_HOME", selected)
	got := CaptureTranscriptHomes()
	if got.OMPHome != selected {
		t.Fatalf("OMPHome = %q, want %q", got.OMPHome, selected)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
