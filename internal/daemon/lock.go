package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// ErrSingletonLockHeld is returned by acquireSingletonLock when another live
// process already holds the lock for the same NM_HOME.
var ErrSingletonLockHeld = errors.New("a no-mistakes daemon is already running for this NM_HOME")

// singletonLock guards NM_HOME against more than one live daemon process.
// It must be acquired before any global, destructive startup operation
// (stale-run recovery, orphan worktree cleanup) and before the IPC socket is
// bound, and held for the lifetime of the process.
//
// The underlying lock (tryLockFile/unlockFile, platform-specific in
// lock_unix.go / lock_windows.go) is the OS's native file lock, which the
// kernel releases automatically when the owning process exits or dies for
// any reason (including SIGKILL) - even without an explicit unlock. That
// self-cleaning property is why this lock needs no separate "is the holder
// still alive" staleness check the way the PID file does: the lock can only
// be held by a process that is actually still running.
type singletonLock struct {
	file *os.File
}

// acquireSingletonLock takes an exclusive, non-blocking OS lock on
// p.LockFile(). If another live daemon already holds it, it returns
// ErrSingletonLockHeld wrapped with whatever diagnostic info (pid, start
// time) the holder recorded, so the caller can report the existing daemon
// instead of silently proceeding.
func acquireSingletonLock(p *paths.Paths) (*singletonLock, error) {
	path := p.LockFile()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock %s: %w", path, err)
	}
	if lockErr := tryLockFile(f); lockErr != nil {
		holder := readLockHolder(f)
		f.Close()
		if holder != "" {
			return nil, fmt.Errorf("%w (%s): %w", ErrSingletonLockHeld, holder, lockErr)
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrSingletonLockHeld, path, lockErr)
	}

	// Best-effort diagnostics: record who holds the lock so a rejected
	// second daemon (or an operator) can identify the live owner. This write
	// is not part of the safety mechanism itself - the OS lock already
	// provides that - so a failure here is non-fatal.
	record := lockHolderRecord{PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if data, err := json.Marshal(record); err == nil {
		_ = f.Truncate(0)
		_, _ = f.WriteAt(data, 0)
		_ = f.Sync()
	}

	return &singletonLock{file: f}, nil
}

// Release drops the lock and closes the underlying file. Safe to call on a
// nil receiver so callers can defer it unconditionally after a failed
// acquire.
func (l *singletonLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockFile(l.file)
	_ = l.file.Close()
}

type lockHolderRecord struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// readLockHolder best-effort reads the diagnostic record left by whichever
// process holds the lock. Returns "" if unavailable or unparsable - the
// caller falls back to a generic message in that case.
func readLockHolder(f *os.File) string {
	data := make([]byte, 4096)
	n, err := f.ReadAt(data, 0)
	if err != nil && n == 0 {
		return ""
	}
	var record lockHolderRecord
	if err := json.Unmarshal(data[:n], &record); err != nil || record.PID <= 0 {
		return ""
	}
	return fmt.Sprintf("pid %d, started %s", record.PID, record.StartedAt.Format(time.RFC3339))
}
