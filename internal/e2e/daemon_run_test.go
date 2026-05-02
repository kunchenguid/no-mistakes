//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonRunUsesProvidedRoot(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	base := os.TempDir()
	if base == "/var/folders" || strings.HasPrefix(base, "/var/folders/") {
		base = "/tmp"
	}
	rootDir, err := os.MkdirTemp(base, "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	wantRoot := filepath.Join(rootDir, "nm-home")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, h.NMBin, "daemon", "run", "--root", wantRoot)
	cmd.Dir = h.WorkDir
	cmd.Env = os.Environ()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon run --root: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		_, _ = h.RunInDirWithEnv(h.WorkDir, map[string]string{"NM_HOME": wantRoot}, "daemon", "stop")
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("daemon run --root exited before becoming healthy: %v\n%s", err, output.String())
		default:
		}

		status, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{"NM_HOME": wantRoot}, "daemon", "status")
		if err == nil && strings.Contains(status, "daemon running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon run --root did not become healthy; last status %q err %v\n%s", status, err, output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	defaultStatus, err := h.Run("daemon", "status")
	if err != nil {
		t.Fatalf("default daemon status: %v\n%s", err, defaultStatus)
	}
	if strings.Contains(defaultStatus, "daemon running") {
		t.Fatalf("daemon run --root should not use default NM_HOME, got status %q", defaultStatus)
	}
}
