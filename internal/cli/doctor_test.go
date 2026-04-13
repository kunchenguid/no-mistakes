package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorSystemSectionHealthyState(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	t.Setenv("NM_FAKE_BIN", "1")
	linkTestBinary(t, binDir, "git")
	linkTestBinary(t, binDir, "gh")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"System",
		"git version 9.9.9",
		"gh",
		"ok",
		nmHome,
		"database",
		"will be created on first use",
		"daemon",
		"stopped",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output should include %q, got: %s", want, out)
		}
	}
	if strings.Contains(out, "some checks failed") {
		t.Fatalf("doctor output should not report failed checks for healthy system state, got: %s", out)
	}
}

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
		"optional, needed for PR/babysit",
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

func TestDoctorAgentsSectionReportsFoundAndMissingBinaries(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	t.Setenv("NM_FAKE_BIN", "1")
	claudePath := linkTestBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatal(err)
	}

	for _, label := range []string{"Agents", "claude", "codex", "rovodev", "opencode"} {
		if !strings.Contains(out, label) {
			t.Fatalf("doctor output should include %q in agents section, got: %s", label, out)
		}
	}
	if !strings.Contains(out, claudePath) {
		t.Fatalf("doctor output should report discovered claude path %q, got: %s", claudePath, out)
	}
	if got := strings.Count(out, "not found"); got < 3 {
		t.Fatalf("doctor output should report missing agent binaries, got %d not found markers in: %s", got, out)
	}
}
