package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteServerPIDFile_WritesJSONInDir(t *testing.T) {
	dir := t.TempDir()
	info := ServerPIDInfo{
		PID:            12345,
		OwnerStartedAt: time.Date(2026, 4, 20, 9, 59, 0, 0, time.UTC),
		Agent:          "opencode",
		Bin:            "/usr/local/bin/opencode",
		Port:           54321,
		StartedAt:      time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
	}

	path := writeServerPIDFile(dir, info)
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if filepath.Dir(path) != dir {
		t.Errorf("path not under dir: %q", path)
	}
	if !strings.Contains(filepath.Base(path), "opencode") || !strings.Contains(filepath.Base(path), "12345") {
		t.Errorf("filename should include agent and pid, got %q", filepath.Base(path))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got ServerPIDInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != info {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, info)
	}
}

func TestWriteServerPIDFile_EmptyDirNoop(t *testing.T) {
	path := writeServerPIDFile("", ServerPIDInfo{PID: 1, Agent: "x"})
	if path != "" {
		t.Errorf("expected empty path when dir disabled, got %q", path)
	}
}

func TestWriteServerPIDFile_CreatesMissingDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "nested", "servers")

	path := writeServerPIDFile(dir, ServerPIDInfo{PID: 2, Agent: "rovodev"})
	if path == "" {
		t.Fatal("expected path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist: %v", err)
	}
}

func TestRemoveServerPIDFile_DeletesAndIgnoresMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeServerPIDFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone, got err=%v", err)
	}
	// Second call on missing file must not panic or error loudly.
	removeServerPIDFile(path)
	removeServerPIDFile("")
}

func TestSetServerPIDsDir_RoundTrip(t *testing.T) {
	prev := currentServerPIDsDir()
	prevOwner := currentServerPIDOwner()
	t.Cleanup(func() { SetServerPIDsDirForOwner(prev, prevOwner) })

	SetServerPIDsDir("/tmp/pids")
	if got := currentServerPIDsDir(); got != "/tmp/pids" {
		t.Errorf("got %q want /tmp/pids", got)
	}
	if got := currentServerPIDOwner(); got != ServerPIDOwnerDaemon {
		t.Errorf("got owner %q want %q", got, ServerPIDOwnerDaemon)
	}
	SetServerPIDsDirForOwner("/tmp/wizard", ServerPIDOwnerWizard)
	if got := currentServerPIDsDir(); got != "/tmp/wizard" {
		t.Errorf("got %q want /tmp/wizard", got)
	}
	if got := currentServerPIDOwner(); got != ServerPIDOwnerWizard {
		t.Errorf("got owner %q want %q", got, ServerPIDOwnerWizard)
	}
	SetServerPIDsDir("")
	if got := currentServerPIDsDir(); got != "" {
		t.Errorf("empty reset, got %q", got)
	}
	if got := currentServerPIDOwner(); got != "" {
		t.Errorf("empty reset owner, got %q", got)
	}
}

func TestWriteServerPIDFile_ConcurrentReadersNeverSeePartialJSON(t *testing.T) {
	dir := t.TempDir()
	info := ServerPIDInfo{
		PID:            12345,
		Owner:          ServerPIDOwnerDaemon,
		OwnerPID:       4321,
		OwnerStartedAt: time.Date(2026, 4, 20, 9, 59, 0, 0, time.UTC),
		Agent:          "opencode",
		Bin:            strings.Repeat("/usr/local/bin/opencode", 1<<15),
		Port:           54321,
		StartedAt:      time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
	}
	path := writeServerPIDFile(dir, info)
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	stop := make(chan struct{})
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- readPIDFileUntilStopped(path, stop)
	}()

	for i := 0; i < 200; i++ {
		info.Port = 54321 + i
		if got := writeServerPIDFile(dir, info); got != path {
			t.Fatalf("writeServerPIDFile() path = %q, want %q", got, path)
		}
	}

	close(stop)
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
}


func TestReadPIDFileUntilStopped_RequiresSuccessfulRead(t *testing.T) {
	stop := make(chan struct{})
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- readPIDFileUntilStopped(filepath.Join(t.TempDir(), "missing.json"), stop)
	}()

	time.Sleep(20 * time.Millisecond)
	close(stop)

	err := <-resultCh
	if !errors.Is(err, errPIDFileNeverRead) {
		t.Fatalf("readPIDFileUntilStopped() error = %v, want %v", err, errPIDFileNeverRead)
	}
}

var errPIDFileNeverRead = errors.New("pid file was never read successfully")

func readPIDFileUntilStopped(path string, stop <-chan struct{}) error {
	var successCount int
	for {
		select {
		case <-stop:
			if successCount == 0 {
				return errPIDFileNeverRead
			}
			return nil
		default:
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) || isTransientPIDOpenError(err) {
				continue
			}
			return fmt.Errorf("read pid file: %w", err)
		}
		var got ServerPIDInfo
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("saw partial pid file: %w", err)
		}
		successCount++
	}
}
