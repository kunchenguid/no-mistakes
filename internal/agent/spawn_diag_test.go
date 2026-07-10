package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactArgv_HidesPromptAndSchema(t *testing.T) {
	args := []string{"--model", "sonnet", "-p", "secret prompt text", "--verbose", "--json-schema", `{"type":"object"}`}
	got := redactArgv(args)

	if strings.Contains(got, "secret prompt text") {
		t.Errorf("prompt leaked into %q", got)
	}
	if strings.Contains(got, `{"type":"object"}`) {
		t.Errorf("schema leaked into %q", got)
	}
	for _, want := range []string{"--model", "sonnet", "-p", "--verbose", "--json-schema", "<redacted len=18>"} {
		if !strings.Contains(got, want) {
			t.Errorf("redactArgv = %q, want to contain %q", got, want)
		}
	}
}

func TestDiagCapture_CountsAllBytesCapsBuffer(t *testing.T) {
	c := &diagCapture{limit: 4}
	io.Copy(c, strings.NewReader("hello world"))

	if c.n != 11 {
		t.Errorf("n = %d, want 11 (all bytes counted)", c.n)
	}
	if c.head() != "hell" {
		t.Errorf("head = %q, want %q (capped at limit)", c.head(), "hell")
	}
}

func TestSpawnDiag_DisabledByDefaultIsNoop(t *testing.T) {
	t.Setenv(spawnDiagEnv, "")
	t.Setenv("NM_HOME", t.TempDir()) // isolate: no sentinel present

	d := newSpawnDiag("claude", "claude", []string{"-p", "x"}, RunOpts{})
	if d.enabled {
		t.Fatal("diag should be disabled when NM_SPAWN_DIAG is empty and no sentinel exists")
	}
	// All methods must be safe on a disabled instance.
	r := strings.NewReader("payload")
	if got := d.wrapStdout(r); got != io.Reader(r) {
		t.Error("wrapStdout should return the reader unchanged when disabled")
	}
	d.logStarted(123)
	d.logStartError(io.EOF)
	d.logExit(123, "ok", nil, true, nil)
}

func TestSpawnDiag_EnabledBySentinelFile(t *testing.T) {
	t.Setenv(spawnDiagEnv, "")
	home := t.TempDir()
	t.Setenv("NM_HOME", home)

	if spawnDiagEnabled() {
		t.Fatal("should be disabled before the sentinel is created")
	}
	if err := os.WriteFile(filepath.Join(home, spawnDiagSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !spawnDiagEnabled() {
		t.Fatal("sentinel file under NM_HOME should enable diagnostics")
	}
}
