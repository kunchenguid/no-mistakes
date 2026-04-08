package cli

import (
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

func TestDoctorAgentCheck(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatal(err)
	}
	// Should check for at least one agent binary.
	if !strings.Contains(out, "claude") {
		t.Errorf("doctor output should check for claude agent, got: %s", out)
	}
}

func TestDoctorHelpListed(t *testing.T) {
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "doctor") {
		t.Errorf("help output should list doctor command, got: %s", out)
	}
}
