package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToRepoRelPOSIX(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "src", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(work, "src", "foo.go"), "package src\n")
	mustWrite(t, filepath.Join(work, "src", "sub", "bar.go"), "package sub\n")

	tests := []struct {
		name    string
		absPath string
		workDir string
		want    string
	}{
		{"absolute under workdir", filepath.Join(work, "src", "foo.go"), work, "src/foo.go"},
		{"trailing separator on workdir", filepath.Join(work, "src", "foo.go"), work + string(filepath.Separator), "src/foo.go"},
		{"nested under workdir", filepath.Join(work, "src", "sub", "bar.go"), work, "src/sub/bar.go"},
		{"already relative", "src/foo.go", work, "src/foo.go"},
		{"dot segments cleaned", filepath.Join(work, "src", "sub", "..", "foo.go"), work, "src/foo.go"},
		{"empty workdir keeps relative", "a/b.go", "", "a/b.go"},
		{"path equals workdir collapses to dot", work, work, "."},
		{"path not under workdir stays absolute", "/other/place/foo.go", work, "/other/place/foo.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := toRepoRelPOSIX(tc.absPath, tc.workDir)
			if got != tc.want {
				t.Errorf("toRepoRelPOSIX(%q, %q) = %q, want %q", tc.absPath, tc.workDir, got, tc.want)
			}
		})
	}
}

// TestToRepoRelPOSIX_SymlinkRoot confirms a path matches whether it (or the
// workDir) is spelled via a symlink or its target. This is the macOS /var vs
// /private/var case called out in AGENTS.md — git may emit one spelling while a
// coverage tool emits the other. Skipped on filesystems without symlink
// support.
func TestToRepoRelPOSIX_SymlinkRoot(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported on this filesystem: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(real, "src", "a.go"), "package src\n")

	absViaReal := filepath.Join(real, "src", "a.go")
	absViaLink := filepath.Join(link, "src", "a.go")

	// workDir spelled via the link, path spelled via the target.
	if got := toRepoRelPOSIX(absViaReal, link); got != "src/a.go" {
		t.Errorf("workDir=link, path=real: toRepoRelPOSIX = %q, want src/a.go", got)
	}
	// workDir spelled via the target, path spelled via the link.
	if got := toRepoRelPOSIX(absViaLink, real); got != "src/a.go" {
		t.Errorf("workDir=real, path=link: toRepoRelPOSIX = %q, want src/a.go", got)
	}
}

func TestNamespaceFindings(t *testing.T) {
	t.Parallel()
	fs := []Finding{
		{ID: uncoveredChangedLinesIDPrefix + "pkg/a.go"},
		{ID: uncoveredChangedLinesIDPrefix + "pkg/b.go"},
	}
	got := namespaceFindings("go", fs)
	want0 := uncoveredChangedLinesIDPrefix + "go:pkg/a.go"
	want1 := uncoveredChangedLinesIDPrefix + "go:pkg/b.go"
	if got[0].ID != want0 || got[1].ID != want1 {
		t.Errorf("namespaceFindings = [%s, %s], want [%s, %s]", got[0].ID, got[1].ID, want0, want1)
	}
	// Bare prefix preserved (downstream prefix matching keeps working).
	for _, f := range got {
		if !strings.HasPrefix(f.ID, uncoveredChangedLinesIDPrefix) {
			t.Errorf("namespaced ID lost its prefix: %s", f.ID)
		}
	}
}
