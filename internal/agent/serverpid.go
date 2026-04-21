package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ServerPIDInfo records a managed server's identity on disk so that a
// freshly started daemon can reap orphaned subprocesses left behind by a
// crashed predecessor. The file is written after the subprocess starts
// and deleted after it shuts down cleanly.
type ServerPIDInfo struct {
	PID       int       `json:"pid"`
	Agent     string    `json:"agent"`
	Bin       string    `json:"bin"`
	Port      int       `json:"port"`
	StartedAt time.Time `json:"started_at"`
}

var (
	serverPIDsDirMu sync.RWMutex
	serverPIDsDir   string
)

// SetServerPIDsDir configures where managed-server PID files are written.
// Callers (typically the daemon at startup) should point this at
// paths.ServerPIDsDir(). Empty string disables PID tracking, which is the
// default for processes that don't own a long-running daemon identity.
func SetServerPIDsDir(dir string) {
	serverPIDsDirMu.Lock()
	defer serverPIDsDirMu.Unlock()
	serverPIDsDir = dir
}

func currentServerPIDsDir() string {
	serverPIDsDirMu.RLock()
	defer serverPIDsDirMu.RUnlock()
	return serverPIDsDir
}

// CurrentServerPIDsDir returns the configured directory for PID tracking
// files, or "" if tracking is disabled.
func CurrentServerPIDsDir() string { return currentServerPIDsDir() }

// writeServerPIDFile serializes info into a uniquely named file under dir
// and returns the file path. When dir is empty the call is a no-op and the
// empty string is returned so callers can treat "no tracking" uniformly.
// Failures are logged but not surfaced because PID tracking is best-effort
// and shouldn't block a server from starting.
func writeServerPIDFile(dir string, info ServerPIDInfo) string {
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("create server pid dir", "dir", dir, "error", err)
		return ""
	}
	name := fmt.Sprintf("%s-%d.json", info.Agent, info.PID)
	path := filepath.Join(dir, name)
	data, err := json.Marshal(info)
	if err != nil {
		slog.Warn("marshal server pid", "error", err)
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("write server pid file", "path", path, "error", err)
		return ""
	}
	return path
}

// removeServerPIDFile deletes path, silently ignoring missing files.
func removeServerPIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove server pid file", "path", path, "error", err)
	}
}
