package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorReportsProviderPlugins(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	global := "agent: codex\nprovider_plugins:\n" +
		"  ssm:\n    command: nm-ssm\n    hosts: [\"*.sourcemanager.dev\"]\n" +
		"  zzz:\n    command: nm-missing-plugin\n    hosts: [git.example.com]\n"
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "codex")
	writeDoctorStubBinary(t, binDir, "nm-ssm")
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor changed its zero-exit reporting contract: %v\n%s", err, out)
	}
	for _, want := range []string{"Provider plugins", "plugin ssm", "*.sourcemanager.dev", "plugin zzz", `command "nm-missing-plugin" not found`, "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}
