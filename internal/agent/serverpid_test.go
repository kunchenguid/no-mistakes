package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	observedInitial := make(chan struct{})
	observedFinal := make(chan struct{})
	resultCh := make(chan error, 1)
	finalPort := info.Port + 199
	go func() {
		resultCh <- readPIDFileUntilStopped(path, info.Port, finalPort, stop, observedInitial, observedFinal)
	}()

	select {
	case <-observedInitial:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never observed initial pid file")
	}

	for i := 0; i < 200; i++ {
		info.Port = 54321 + i
		if got := writeServerPIDFile(dir, info); got != path {
			t.Fatalf("writeServerPIDFile() path = %q, want %q", got, path)
		}
	}

	select {
	case <-observedFinal:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never observed final pid file rewrite")
	}

	close(stop)
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
}

func TestWriteServerPIDFile_RetriesTransientRenameError(t *testing.T) {
	dir := t.TempDir()
	prevRename := renameServerPIDFile
	prevSleep := sleepServerPIDRenameRetry
	prevTransient := isTransientPIDRenameError
	t.Cleanup(func() {
		renameServerPIDFile = prevRename
		sleepServerPIDRenameRetry = prevSleep
		isTransientPIDRenameError = prevTransient
	})

	var calls int
	renameServerPIDFile = func(oldpath, newpath string) error {
		calls++
		if calls < 3 {
			return &fs.PathError{Op: "rename", Path: newpath, Err: errors.New("transient")}
		}
		return os.Rename(oldpath, newpath)
	}
	sleepServerPIDRenameRetry = func() {}
	isTransientPIDRenameError = func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "transient")
	}

	path := writeServerPIDFile(dir, ServerPIDInfo{PID: 7, Agent: "opencode"})
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if calls != 3 {
		t.Fatalf("rename calls = %d, want 3", calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected pid file to exist: %v", err)
	}
}

func TestReadPIDFileUntilStopped_RequiresSuccessfulRead(t *testing.T) {
	stop := make(chan struct{})
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- readPIDFileUntilStopped(filepath.Join(t.TempDir(), "missing.json"), 0, 0, stop, nil, nil)
	}()

	time.Sleep(20 * time.Millisecond)
	close(stop)

	err := <-resultCh
	if !errors.Is(err, errPIDFileNeverRead) {
		t.Fatalf("readPIDFileUntilStopped() error = %v, want %v", err, errPIDFileNeverRead)
	}
}

func TestReadPIDFileUntilStopped_RequiresUpdatedRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.json")
	info := ServerPIDInfo{
		PID:            12345,
		Owner:          ServerPIDOwnerDaemon,
		OwnerPID:       4321,
		OwnerStartedAt: time.Date(2026, 4, 20, 9, 59, 0, 0, time.UTC),
		Agent:          "opencode",
		Bin:            "/usr/local/bin/opencode",
		Port:           54321,
		StartedAt:      time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	observedInitial := make(chan struct{})
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- readPIDFileUntilStopped(path, info.Port, info.Port+1, stop, observedInitial, nil)
	}()

	select {
	case <-observedInitial:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never observed initial pid file")
	}
	close(stop)

	err = <-resultCh
	if !errors.Is(err, errPIDFileNeverObservedRewrite) {
		t.Fatalf("readPIDFileUntilStopped() error = %v, want %v", err, errPIDFileNeverObservedRewrite)
	}
}

var errPIDFileNeverRead = errors.New("pid file was never read successfully")
var errPIDFileNeverObservedRewrite = errors.New("pid file rewrite was never read successfully")

func readPIDFileUntilStopped(path string, initialPort int, finalPort int, stop <-chan struct{}, observedInitial chan<- struct{}, observedFinal chan<- struct{}) error {
	var successCount int
	var sawRewrite bool
	var sentInitial bool
	var sawFinal bool
	var sentFinal bool
	for {
		select {
		case <-stop:
			if successCount == 0 {
				return errPIDFileNeverRead
			}
			if !sawRewrite || (finalPort != initialPort && !sawFinal) {
				return errPIDFileNeverObservedRewrite
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
		if got.Port == initialPort && !sentInitial {
			sentInitial = true
			if observedInitial != nil {
				close(observedInitial)
			}
		}
		if got.Port != initialPort {
			sawRewrite = true
		}
		if got.Port == finalPort {
			sawFinal = true
			if !sentFinal {
				sentFinal = true
				if observedFinal != nil {
					close(observedFinal)
				}
			}
		}
	}
}
