package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorSystemSectionHealthyState(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	for name, body := range map[string]string{
		"git": "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  printf 'git version 9.9.9\\n'\n  exit 0\nfi\nexit 1\n",
		"gh":  "#!/bin/sh\nexit 0\n",
	} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
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
