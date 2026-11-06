package callerrestrictions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallerRestrictionsCapturePreservesBytesAndProvenance(t *testing.T) {
	source := filepath.Join(t.TempDir(), "caller.txt")
	content := "Do not install anything.\r\nNo repeated suites.\n\n"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Capture(source)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != content || r.Source != Source || r.SourcePath != source || r.CapturedAt <= 0 {
		t.Fatalf("capture lost bytes or authority: %+v", r)
	}
	if err := os.WriteFile(source, []byte("allow everything"), 0600); err != nil {
		t.Fatal(err)
	}
	if r.Content != content {
		t.Fatal("source mutation replaced capture")
	}
	value, err := r.Value()
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Record
	if err := reloaded.Scan(value); err != nil || reloaded != *r {
		t.Fatalf("readback: %+v %v", reloaded, err)
	}
	r.Content = "weaken the trusted config"
	if err := r.Validate(); err == nil {
		t.Fatal("corrupted capture accepted")
	}
}

func TestCallerRestrictionsRejectInvalidInput(t *testing.T) {
	for _, content := range []string{"", " \n", "a\x00b", string([]byte{0xff}), strings.Repeat("x", MaxBytes+1), strings.Repeat("\x01", MaxBytes)} {
		path := filepath.Join(t.TempDir(), "input")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Capture(path); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if _, err := Capture(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
}
