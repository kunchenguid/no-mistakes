package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// TestTestEvidenceDir_ReadsTheExecutorResolvedDirectory pins the step side of
// the evidence path to whatever the executor resolved, rather than to a path
// the step rebuilds for itself. Steps used to join os.TempDir() directly, which
// on Linux is the shared /tmp - RAM-backed on current Ubuntu, and reaped by
// nobody in this program.
func TestTestEvidenceDir_ReadsTheExecutorResolvedDirectory(t *testing.T) {
	want := filepath.Join(t.TempDir(), "evidence", "run-123")
	got := testEvidenceDir(&pipeline.StepContext{EvidenceDir: want})
	if got != want {
		t.Errorf("evidence dir = %q, want %q", got, want)
	}
}

// TestTestEvidenceDir_DefaultResolutionStaysUnderTheAppRoot walks the exact
// production path - app root to evidence directory - and asserts it lands under
// NM_HOME and not in the shared system temp directory the old code used.
func TestTestEvidenceDir_DefaultResolutionStaysUnderTheAppRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}

	got := testEvidenceDir(&pipeline.StepContext{EvidenceDir: p.RunEvidenceDir("", "run-123")})

	want := filepath.Join(root, "evidence", "run-123")
	if got != want {
		t.Errorf("evidence dir = %q, want %q", got, want)
	}
	legacy := filepath.Join(os.TempDir(), "no-mistakes-evidence")
	if strings.HasPrefix(got, legacy) {
		t.Errorf("evidence dir %q is still under the legacy shared temp root %q", got, legacy)
	}
}

func TestEvidenceBranchSlug_KeepsBranchStructureAndDropsUnsafeSegments(t *testing.T) {
	cases := map[string]string{
		"feature/add-login":  "feature/add-login",
		"../../etc/pa ss~wd": "etc/pa-ss-wd",
		"///":                "",
	}
	for branch, want := range cases {
		got := strings.Join(evidenceBranchSlug(branch), "/")
		if got != want {
			t.Errorf("slug(%q) = %q, want %q", branch, got, want)
		}
	}
}
