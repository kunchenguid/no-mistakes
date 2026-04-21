package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// orphanStartTimeTolerance bounds the acceptable difference between the
// process start time recorded in a PID file and the one reported by the
// kernel now. A small nonzero value absorbs clock quirks and the sub-second
// gap between cmd.Start() and the time.Now() we record.
const orphanStartTimeTolerance = 2 * time.Second

var processRunningFunc = processRunning
var processStartTimeFunc = processStartTime
var terminateOrphanProcessGroupFunc = terminateOrphanProcessGroup

type daemonPIDFile struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// reapOrphanedServers kills managed-server subprocesses (opencode,
// rovodev) left behind by a crashed predecessor daemon and deletes their
// stale PID files.
//
// Safety rules:
//   - If another no-mistakes daemon is still running, skip everything so
//     we don't kill that daemon's live servers.
//   - For each PID file, require the recorded StartedAt to match the
//     process's actual start time within orphanStartTimeTolerance. If not,
//     the PID has been reused by something unrelated; delete the file but
//     do not signal the process.
func reapOrphanedServers(p *paths.Paths) {
	dir := p.ServerPIDsDir()
	if otherDaemonAlive(p) {
		slog.Info("another daemon appears to be running; skipping managed-server reap", "dir", dir)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("read server pids dir", "dir", dir, "error", err)
		}
		return
	}
	myPID := os.Getpid()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, ok := readServerPIDRecord(path)
		if !ok {
			removeServerPIDFile(path)
			continue
		}
		if info.PID <= 0 || info.PID == myPID {
			removeServerPIDFile(path)
			continue
		}
		if shouldSkipOrphanRecord(info) {
			continue
		}
		alive, err := processRunningFunc(info.PID)
		if err != nil {
			slog.Warn("check orphaned server", "pid", info.PID, "error", err)
			continue
		}
		if !alive {
			removeServerPIDFile(path)
			continue
		}
		matches, err := orphanStartTimeMatches(info)
		if err != nil {
			slog.Warn("check orphan start time", "pid", info.PID, "error", err)
			continue
		}
		if !matches {
			slog.Info("orphan pid file stale; pid reused by unrelated process, not killing",
				"pid", info.PID, "agent", info.Agent)
			removeServerPIDFile(path)
			continue
		}
		slog.Info("reaping orphaned managed server", "pid", info.PID, "agent", info.Agent, "bin", info.Bin)
		if err := terminateOrphanProcessGroupFunc(info.PID); err != nil {
			slog.Warn("terminate orphan", "pid", info.PID, "error", err)
			continue
		}
		removeServerPIDFile(path)
	}
}

func shouldSkipOrphanRecord(info agent.ServerPIDInfo) bool {
	if info.Owner == "" || info.Owner == agent.ServerPIDOwnerDaemon {
		return false
	}
	if info.Owner != agent.ServerPIDOwnerWizard {
		return true
	}
	if info.OwnerPID <= 0 {
		return true
	}
	alive, err := processRunningFunc(info.OwnerPID)
	if err != nil {
		slog.Warn("check wizard owner pid", "pid", info.OwnerPID, "error", err)
		return true
	}
	if !alive {
		return false
	}
	startedAt, err := processStartTimeFunc(info.OwnerPID)
	if err != nil {
		slog.Warn("check wizard owner start time", "pid", info.OwnerPID, "error", err)
		return true
	}
	if info.OwnerStartedAt.IsZero() {
		return true
	}
	diff := startedAt.Sub(info.OwnerStartedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff <= orphanStartTimeTolerance
}

func readServerPIDRecord(path string) (agent.ServerPIDInfo, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ServerPIDInfo{}, false
	}
	var info agent.ServerPIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return agent.ServerPIDInfo{}, false
	}
	return info, true
}

func removeServerPIDFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("remove server pid file", "path", path, "error", err)
	}
}

// otherDaemonAlive returns true if the daemon PID file points at a running
// process that isn't us. The recovery path runs before the new daemon
// writes its own PID file, so any live PID here belongs to a predecessor.
func otherDaemonAlive(p *paths.Paths) bool {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("read daemon pid file", "path", p.PIDFile(), "error", err)
			return true
		}
		return false
	}
	if record.PID == os.Getpid() {
		return false
	}
	if record.PID <= 0 {
		slog.Warn("invalid daemon pid", "path", p.PIDFile(), "pid", record.PID)
		return true
	}
	alive, err := processRunningFunc(record.PID)
	if err != nil {
		slog.Warn("check daemon pid", "pid", record.PID, "error", err)
		return true
	}
	if !alive {
		return false
	}
	startedAt, err := processStartTimeFunc(record.PID)
	if err != nil {
		slog.Warn("check daemon start time", "pid", record.PID, "error", err)
		return true
	}
	matches, err := daemonPIDRecordMatchesProcess(p.PIDFile(), record, startedAt)
	if err != nil {
		slog.Warn("validate daemon pid", "path", p.PIDFile(), "pid", record.PID, "error", err)
		return true
	}
	return matches
}

func readDaemonPIDFile(path string) (daemonPIDFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonPIDFile{}, err
	}
	return readDaemonPIDFileData(data)
}

func readDaemonPIDFileData(data []byte) (daemonPIDFile, error) {
	var record daemonPIDFile
	if err := json.Unmarshal(data, &record); err == nil {
		if record.PID <= 0 {
			return daemonPIDFile{}, fmt.Errorf("invalid pid file: pid must be positive")
		}
		return record, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return daemonPIDFile{}, fmt.Errorf("invalid pid file: %w", err)
	}
	if pid <= 0 {
		return daemonPIDFile{}, fmt.Errorf("invalid pid file: pid must be positive")
	}
	return daemonPIDFile{PID: pid}, nil
}

func orphanStartTimeMatches(info agent.ServerPIDInfo) (bool, error) {
	actual, err := processStartTimeFunc(info.PID)
	if err != nil {
		return false, err
	}
	diff := actual.Sub(info.StartedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff <= orphanStartTimeTolerance, nil
}

func daemonPIDRecordMatchesProcess(path string, record daemonPIDFile, actualStart time.Time) (bool, error) {
	expectedStart := record.StartedAt.UTC()
	if expectedStart.IsZero() {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat pid file: %w", err)
		}
		expectedStart = info.ModTime().UTC()
	}
	diff := actualStart.Sub(expectedStart)
	if diff < 0 {
		diff = -diff
	}
	return diff <= orphanStartTimeTolerance, nil
}
