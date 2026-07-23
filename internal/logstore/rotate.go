// Package logstore owns byte bounds and deterministic retention for process logs.
package logstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Policy defines the hard size of the current file and how many same-sized
// backups are retained. Backups use the suffixes .1 (newest) through .N.
type Policy struct {
	MaxBytes int64
	Backups  int
}

// Log bounds have one owner so lifecycle, managed dependency, and bootstrap
// sinks cannot silently drift apart. Functions return values so callers cannot
// mutate process-wide policy.
func LifecyclePolicy() Policy {
	return Policy{MaxBytes: 32 << 20, Backups: 3}
}

func ManagedServerPolicy() Policy {
	return Policy{MaxBytes: 16 << 20, Backups: 2}
}

func BootstrapPolicy() Policy {
	return Policy{MaxBytes: 1 << 20, Backups: 2}
}

// RotatingWriter is a concurrent-safe, byte-bounded writer. Rotation copies
// the current file into the backup chain and truncates the current inode in
// place. Keeping the inode is important for service managers and subprocesses
// that may still hold an open descriptor for the current path.
type RotatingWriter struct {
	mu     sync.Mutex
	path   string
	policy Policy
	file   *os.File
	size   int64
	closed bool
}

// Open opens path for append and immediately brings any pre-existing files
// under policy. Directories are created with the project-standard mode.
func Open(path string, policy Policy) (*RotatingWriter, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	w := &RotatingWriter{path: path, policy: policy, file: file}
	if err := w.normalizeExistingLocked(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return w, nil
}

func validatePolicy(policy Policy) error {
	if policy.MaxBytes <= 0 {
		return fmt.Errorf("log max bytes must be positive")
	}
	if policy.Backups < 0 {
		return fmt.Errorf("log backups must be non-negative")
	}
	return nil
}

// Write appends all of p, rotating before each chunk that would exceed the
// current-file bound. A single oversized write is split across rotations, so
// no file exceeds MaxBytes even transiently after Write returns.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}

	written := 0
	for len(p) > 0 {
		if w.size >= w.policy.MaxBytes {
			if err := w.rotateLocked(); err != nil {
				return written, err
			}
		}
		remaining := w.policy.MaxBytes - w.size
		chunk := int64(len(p))
		if chunk > remaining {
			chunk = remaining
		}
		n, err := w.file.Write(p[:int(chunk)])
		written += n
		w.size += int64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
		if int64(n) != chunk {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// RotateNow snapshots a non-empty current file and truncates its inode in
// place. It is used at process start for the small pre-logger crash sink.
func (w *RotatingWriter) RotateNow() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	if w.size == 0 {
		return nil
	}
	return w.rotateLocked()
}

// Close closes the writer. It is safe to call more than once.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// RotateAtStartup bounds existing bootstrap/crash output, moves the latest
// non-empty process output into retention, and leaves the current inode empty
// for descriptors the service manager already opened.
func RotateAtStartup(path string, policy Policy) error {
	w, err := Open(path, policy)
	if err != nil {
		return err
	}
	defer w.Close()
	return w.RotateNow()
}

func (w *RotatingWriter) rotateLocked() error {
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync current log: %w", err)
	}
	if err := shiftBackups(w.path, w.policy.Backups); err != nil {
		return err
	}
	if w.policy.Backups > 0 && w.size > 0 {
		if err := copySectionAtomic(w.file, 0, w.size, backupPath(w.path, 1)); err != nil {
			return fmt.Errorf("snapshot current log: %w", err)
		}
	}
	if err := w.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate current log: %w", err)
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek current log: %w", err)
	}
	w.size = 0
	return nil
}

func (w *RotatingWriter) normalizeExistingLocked() error {
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("stat current log: %w", err)
	}
	w.size = info.Size()
	if err := pruneBackups(w.path, w.policy.Backups); err != nil {
		return err
	}
	for i := 1; i <= w.policy.Backups; i++ {
		if err := trimFileTail(backupPath(w.path, i), w.policy.MaxBytes); err != nil {
			return fmt.Errorf("bound backup %d: %w", i, err)
		}
	}
	if w.size <= w.policy.MaxBytes {
		return nil
	}

	// Preserve the newest retention window from an old unbounded file without
	// reading the whole file into memory. Backup segments are copied first;
	// only the newest current-file segment needs to be buffered before the
	// current inode is truncated in place.
	totalSegments := w.policy.Backups + 1
	availableSegments := int((w.size + w.policy.MaxBytes - 1) / w.policy.MaxBytes)
	if availableSegments < totalSegments {
		totalSegments = availableSegments
	}
	for backup := totalSegments - 1; backup >= 1; backup-- {
		end := w.size - int64(backup)*w.policy.MaxBytes
		start := end - w.policy.MaxBytes
		if start < 0 {
			start = 0
		}
		if err := copySectionAtomic(w.file, start, end-start, backupPath(w.path, backup)); err != nil {
			return fmt.Errorf("compact backup %d: %w", backup, err)
		}
	}
	for i := totalSegments; i <= w.policy.Backups; i++ {
		if err := removeIfExists(backupPath(w.path, i)); err != nil {
			return err
		}
	}

	latestStart := w.size - w.policy.MaxBytes
	latest := make([]byte, w.policy.MaxBytes)
	if _, err := w.file.ReadAt(latest, latestStart); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read latest log segment: %w", err)
	}
	if err := w.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate oversized current log: %w", err)
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek compacted current log: %w", err)
	}
	if _, err := w.file.Write(latest); err != nil {
		return fmt.Errorf("write compacted current log: %w", err)
	}
	w.size = int64(len(latest))
	return nil
}

func shiftBackups(path string, backups int) error {
	if backups <= 0 {
		return pruneBackups(path, 0)
	}
	if err := removeIfExists(backupPath(path, backups)); err != nil {
		return fmt.Errorf("remove oldest backup: %w", err)
	}
	for i := backups - 1; i >= 1; i-- {
		from := backupPath(path, i)
		to := backupPath(path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("shift backup %d: %w", i, err)
		}
	}
	return pruneBackups(path, backups)
}

func copySectionAtomic(src *os.File, offset, length int64, target string) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := io.CopyN(tmp, io.NewSectionReader(src, offset, length), length); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	committed = true
	return nil
}

func trimFileTail(path string, maxBytes int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= maxBytes {
		return nil
	}
	buf := make([]byte, maxBytes)
	if _, err := file.ReadAt(buf, info.Size()-maxBytes); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = file.Write(buf)
	return err
}

func pruneBackups(path string, keep int) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), prefix))
		if err != nil || n <= keep {
			continue
		}
		if err := os.Remove(filepath.Join(filepath.Dir(path), entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func backupPath(path string, index int) string {
	return path + "." + strconv.Itoa(index)
}
