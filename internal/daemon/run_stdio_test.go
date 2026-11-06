package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// Issue #1653: the bootstrap-log fallback is only for output without a usable
// handle. A daemon started with real stdout/stderr (the detached start path's
// bootstrap log file, a terminal, a supervisor's pipes) keeps writing there.
func TestPrepareRunStdioLeavesUsableOutputAlone(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "NM_HOME="+root, "NM_DAEMON_HELPER_PROCESS=run-stdio")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	if !strings.Contains(stdout.String(), "stdout-marker\n") {
		t.Fatalf("stdout = %q, want the stdout marker", stdout.String())
	}
	for _, marker := range []string{"stderr-marker\n", "runtime-stderr-marker\n", "child-stderr-marker\n"} {
		if !strings.Contains(stderr.String(), marker) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), marker)
		}
	}
	if _, err := os.Stat(paths.WithRoot(root).DaemonBootstrapLog()); !os.IsNotExist(err) {
		t.Fatalf("bootstrap log was opened for usable output: %v", err)
	}
}
