package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorBasic(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "git") {
		t.Errorf("doctor output should check git, got: %s", out)
	}
	if !strings.Contains(out, "data directory") {
		t.Errorf("doctor output should check data directory, got: %s", out)
	}
	if !strings.Contains(out, "database") {
		t.Errorf("doctor output should check database, got: %s", out)
	}
	if !strings.Contains(out, "daemon") {
		t.Errorf("doctor output should check daemon, got: %s", out)
	}
}

func TestDoctorGitMissing(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	// Override PATH to make git unavailable.
	t.Setenv("PATH", "/nonexistent")

	out, err := executeCmd("doctor")
	// Doctor should not error — it reports issues inline.
	if err != nil {
		t.Fatalf("doctor should not error even with problems: %v", err)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("doctor should report git not found, got: %s", out)
	}
}

func TestDoctorDataDirMissing(t *testing.T) {
	nmHome := "/nonexistent/path/no-mistakes"
	t.Setenv("NM_HOME", nmHome)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor should not error: %v", err)
	}
	if !strings.Contains(out, "not found") || !strings.Contains(out, "data directory") {
		t.Errorf("doctor should report data directory not found, got: %s", out)
	}
}

func TestDoctorAgentsSectionReportsFoundAndMissingBinaries(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
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
