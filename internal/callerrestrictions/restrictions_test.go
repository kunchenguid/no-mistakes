package callerrestrictions

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallerRestrictionsDigestBindsBytesAndEveryProvenanceField(t *testing.T) {
	var unset *Record
	if unset.Digest() != "" {
		t.Fatal("unset record has an attachment identity")
	}
	record := Record{Source: Source, SourcePath: "/caller.txt", Content: "No downloads.\n", SHA256: strings.Repeat("a", 64), CapturedAt: 1}
	digest := record.Digest()
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != 32 {
		t.Fatalf("record identity must be a bounded SHA-256 digest: %q", digest)
	}
	copy := record
	if copy.Digest() != digest {
		t.Fatal("identical captured record changed attachment identity")
	}
	for _, field := range []string{"source", "source_path", "content", "sha256", "captured_at"} {
		t.Run(field, func(t *testing.T) {
			changed := record
			switch field {
			case "source":
				changed.Source = "other authority"
			case "source_path":
				changed.SourcePath = "/other-caller.txt"
			case "content":
				changed.Content += "No repeated tests.\n"
			case "sha256":
				changed.SHA256 = strings.Repeat("b", 64)
			case "captured_at":
				changed.CapturedAt++
			}
			if changed.Digest() == digest {
				t.Fatal("different captured record retained attachment identity")
			}
		})
	}
}

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
