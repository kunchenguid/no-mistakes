package procguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinDir(t *testing.T) {
	want := filepath.Join("/nm/home", "procguard", "bin")
	if got := BinDir("/nm/home"); got != want {
		t.Fatalf("BinDir = %q, want %q", got, want)
	}
}

func TestAugmentPATH_PrependsAndIsIdempotent(t *testing.T) {
	sep := string(os.PathListSeparator)
	bin := filepath.Join("/nm/home", "procguard", "bin")
	env := []string{"FOO=bar", "PATH=/usr/bin" + sep + "/bin", "BAZ=qux"}

	got := AugmentPATH(env, bin)
	path := ""
	for _, e := range got {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		}
	}
	wantPrefix := bin + sep
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("PATH = %q, want it to start with %q", path, wantPrefix)
	}
	if !strings.Contains(path, "/usr/bin") || !strings.Contains(path, "/bin") {
		t.Fatalf("PATH = %q, want original entries preserved", path)
	}

	// Idempotent: applying again must not double-prepend.
	got2 := AugmentPATH(got, bin)
	path2 := ""
	for _, e := range got2 {
		if strings.HasPrefix(e, "PATH=") {
			path2 = strings.TrimPrefix(e, "PATH=")
		}
	}
	if strings.Count(path2, bin) != 1 {
		t.Fatalf("PATH = %q, want %q exactly once after re-apply", path2, bin)
	}
}

func TestAugmentPATH_SynthesizesMissingPath(t *testing.T) {
	bin := "/nm/home/procguard/bin"
	got := AugmentPATH([]string{"FOO=bar"}, bin)
	found := false
	for _, e := range got {
		if e == "PATH="+bin {
			found = true
		}
	}
	if !found {
		t.Fatalf("want synthesized PATH=%s, got %v", bin, got)
	}
}

func TestAugmentPATH_EmptyBinDirIsNoop(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	got := AugmentPATH(env, "")
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Fatalf("empty binDir must be a no-op, got %v", got)
	}
}
