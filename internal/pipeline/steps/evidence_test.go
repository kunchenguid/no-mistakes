package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestEvidenceDir_AlwaysOutsideTheWorktree(t *testing.T) {
	got := testEvidenceDir("run-123")
	want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	if got != want {
		t.Errorf("evidence dir = %q, want %q", got, want)
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
