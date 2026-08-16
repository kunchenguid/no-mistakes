//go:build unix

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRun_TrimsOutputWithLifecycleAwareRunner(t *testing.T) {
	installFakeGit(t, `printf '  normal output  \n'`)

	out, err := Run(context.Background(), t.TempDir(), "status")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := out, "normal output"; got != want {
		t.Fatalf("Run() output = %q, want %q", got, want)
	}
}

func TestRun_PreservesStderrWithLifecycleAwareRunner(t *testing.T) {
	installFakeGit(t, `printf 'fetch failed\n' >&2; exit 2`)

	_, err := Run(context.Background(), t.TempDir(), "fetch")
	if err == nil {
		t.Fatal("Run() error = nil, want fake git failure")
	}
	if !strings.Contains(err.Error(), "fetch failed") {
		t.Fatalf("Run() error = %q, want captured stderr", err)
	}
}

func TestRun_CancellationStopsPipeHoldingDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	readyFile := filepath.Join(dir, "ready")
	t.Setenv("NM_GIT_TEST_PID_FILE", pidFile)
	t.Setenv("NM_GIT_TEST_READY_FILE", readyFile)
	installFakeGit(t, `
(
	while :; do
		sleep 1
	done
) &
descendant=$!
printf '%s\n' "$descendant" > "$NM_GIT_TEST_PID_FILE"
printf 'ready\n' > "$NM_GIT_TEST_READY_FILE"
wait "$descendant"
`)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Run(ctx, dir, "fetch", "origin", "main")
		result <- err
	}()

	waitForFile(t, readyFile, 5*time.Second)
	descendantPID := readTestPID(t, pidFile)
	t.Cleanup(func() {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want wrapped %v", err, context.Canceled)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return promptly after cancellation")
	}

	if !processGoneWithin(descendantPID, 5*time.Second) {
		t.Fatalf("git descendant %d still exists after Run() returned", descendantPID)
	}
}

func installFakeGit(t *testing.T, body string) {
	t.Helper()
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readTestPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid descendant pid %q: %v", b, err)
	}
	return pid
}

func processGoneWithin(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
