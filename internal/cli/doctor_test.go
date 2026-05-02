package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorSystemSectionReportsMissingGitAndDataDirFailures(t *testing.T) {
	nmHome := filepath.Join(t.TempDir(), "missing-nm-home")
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", "/nonexistent")

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor should not error even when checks fail: %v", err)
	}

	for _, want := range []string{
		"System",
		"git",
		"not found",
		"gh",
		"optional, needed for PR/CI",
		"data directory",
		nmHome,
		"database",
		"will be created on first use",
		"daemon",
		"stopped",
		"some checks failed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output should include %q, got: %s", want, out)
		}
	}
}
