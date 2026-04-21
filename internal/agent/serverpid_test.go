package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteServerPIDFile_WritesJSONInDir(t *testing.T) {
	dir := t.TempDir()
	info := ServerPIDInfo{
		PID:       12345,
		Agent:     "opencode",
		Bin:       "/usr/local/bin/opencode",
		Port:      54321,
		StartedAt: time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
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
	t.Cleanup(func() { SetServerPIDsDir(prev) })

	SetServerPIDsDir("/tmp/pids")
	if got := currentServerPIDsDir(); got != "/tmp/pids" {
		t.Errorf("got %q want /tmp/pids", got)
	}
	SetServerPIDsDir("")
	if got := currentServerPIDsDir(); got != "" {
		t.Errorf("empty reset, got %q", got)
	}
}
