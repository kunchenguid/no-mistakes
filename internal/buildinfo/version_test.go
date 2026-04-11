package buildinfo

import "testing"

func TestDefaults(t *testing.T) {
	if Version != "dev" {
		t.Errorf("expected default Version=%q, got %q", "dev", Version)
	}
	if Commit != "unknown" {
		t.Errorf("expected default Commit=%q, got %q", "unknown", Commit)
	}
	if Date != "unknown" {
		t.Errorf("expected default Date=%q, got %q", "unknown", Date)
	}
}

func TestString(t *testing.T) {
	want := "dev (unknown) unknown"
	if got := String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
