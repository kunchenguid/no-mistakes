package e2edaemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Slot is a held concurrency permit for one temporary E2E daemon owner.
type Slot struct {
	inv  *Inventory
	path string
	n    int
}

// MaxConcurrent returns the configured concurrency cap.
func MaxConcurrent() int {
	raw := os.Getenv(EnvMaxConcurrent)
	if raw == "" {
		return DefaultMaxConcurrent
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultMaxConcurrent
	}
	return n
}

// AcquireSlot blocks until a concurrency slot is free or timeout elapses.
// The slot file records the owner pid for diagnostics.
func (inv *Inventory) AcquireSlot(timeout time.Duration) (*Slot, error) {
	if inv == nil {
		return nil, fmt.Errorf("e2edaemon: nil inventory")
	}
	max := MaxConcurrent()
	deadline := time.Now().Add(timeout)
	if timeout <= 0 {
		deadline = time.Now().Add(2 * time.Minute)
	}
	for {
		for i := 0; i < max; i++ {
			path := filepath.Join(inv.Dir, slotsDirName, fmt.Sprintf("slot-%d.lock", i))
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return &Slot{inv: inv, path: path, n: i}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("e2edaemon: concurrency cap %d reached (set %s to raise)", max, EnvMaxConcurrent)
		}
		// Drop clearly stale slot files (owner process gone) so a crashed
		// harness cannot permanently exhaust the cap.
		inv.reclaimStaleSlots(max)
		time.Sleep(50 * time.Millisecond)
	}
}

// Release frees the concurrency slot.
func (s *Slot) Release() {
	if s == nil || s.path == "" {
		return
	}
	_ = os.Remove(s.path)
	s.path = ""
}

func (inv *Inventory) reclaimStaleSlots(max int) {
	for i := 0; i < max; i++ {
		path := filepath.Join(inv.Dir, slotsDirName, fmt.Sprintf("slot-%d.lock", i))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(string(bytesTrimSpace(data)))
		if err != nil || pid <= 0 {
			_ = os.Remove(path)
			continue
		}
		alive, err := processAlive(pid)
		if err != nil || !alive {
			_ = os.Remove(path)
		}
	}
}

func bytesTrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}
