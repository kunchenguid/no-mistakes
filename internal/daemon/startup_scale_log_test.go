package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Parent IPC readiness can observe ServeReady before the child emits
// phase=ipc_health. This pins the poll helper so that race cannot flake CI.
func TestWaitForDaemonLogFragments_AcceptsLateIpcHealth(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	early := []byte("phase=ipc_bind duration_ms=0\n")
	if err := os.WriteFile(logPath, early, 0o644); err != nil {
		t.Fatal(err)
	}

	late := append(early, []byte(
		"phase=ipc_health duration_ms=1\nmsg=\"daemon ready\" pid=1\n",
	)...)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(logPath, late, 0o644)
	}()

	got := waitForDaemonLogFragments(t, logPath, []string{
		"phase=ipc_bind",
		"phase=ipc_health",
		`msg="daemon ready"`,
	}, time.Second)
	if string(got) != string(late) {
		t.Fatalf("log = %q, want late contents", got)
	}
}
