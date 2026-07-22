package e2edaemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// EnvInventory is the absolute path to the suite inventory directory.
	// When unset, a process-local default under the system temp dir is used.
	EnvInventory = "NM_E2E_DAEMON_INVENTORY"

	// EnvMaxConcurrent caps how many temporary E2E daemons may be live at once.
	EnvMaxConcurrent = "NM_E2E_DAEMON_MAX"

	// DefaultMaxConcurrent bounds blast radius of one interrupted suite.
	DefaultMaxConcurrent = 2

	inventoryFileName = "inventory.json"
	lockFileName      = "inventory.lock"
	ownerFileName     = "owner.pid"
	slotsDirName      = "slots"
)

// Entry is one owned temporary E2E daemon root.
type Entry struct {
	ID           string    `json:"id"`
	NMHome       string    `json:"nm_home"`
	PID          int       `json:"pid"`
	NMBin        string    `json:"nm_bin,omitempty"`
	OwnerPID     int       `json:"owner_pid"`
	RegisteredAt time.Time `json:"registered_at"`
}

type inventoryFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Inventory is a process-safe, file-backed record of temporary E2E daemons.
type Inventory struct {
	Dir string

	mu          sync.Mutex
	findDaemons func(string) ([]int, error)
}

// DirFromEnv returns the inventory directory from NM_E2E_DAEMON_INVENTORY,
// or a stable per-user temp default when unset.
func DirFromEnv() string {
	if dir := os.Getenv(EnvInventory); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "no-mistakes-e2e-daemon-inventory")
}

// Open returns an Inventory rooted at DirFromEnv(), creating the directory
// with mode 0700 when needed.
func Open() (*Inventory, error) {
	return OpenDir(DirFromEnv())
}

// OpenDir opens (or creates) an inventory directory.
func OpenDir(dir string) (*Inventory, error) {
	if dir == "" {
		return nil, fmt.Errorf("e2edaemon: empty inventory dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("e2edaemon: mkdir inventory: %w", err)
	}
	// Tighten perms if the directory already existed with a looser mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("e2edaemon: chmod inventory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, slotsDirName), 0o700); err != nil {
		return nil, fmt.Errorf("e2edaemon: mkdir slots: %w", err)
	}
	return &Inventory{Dir: dir, findDaemons: FindDaemonsForRoot}, nil
}

func (inv *Inventory) daemonsForRoot(nmHome string) ([]int, error) {
	if inv.findDaemons != nil {
		return inv.findDaemons(nmHome)
	}
	return FindDaemonsForRoot(nmHome)
}

func (inv *Inventory) path() string {
	return filepath.Join(inv.Dir, inventoryFileName)
}

func (inv *Inventory) lockPath() string {
	return filepath.Join(inv.Dir, lockFileName)
}

// Register records or updates ownership for nmHome. pid may be 0 when the
// daemon has not started yet; SyncPID upgrades it later.
func (inv *Inventory) Register(id, nmHome, nmBin string, pid, ownerPID int) error {
	if inv == nil {
		return fmt.Errorf("e2edaemon: nil inventory")
	}
	nmHome = filepath.Clean(nmHome)
	if nmHome == "" || nmHome == "." {
		return fmt.Errorf("e2edaemon: invalid nm_home")
	}
	if id == "" {
		return fmt.Errorf("e2edaemon: empty entry id")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.withLock(func(file *inventoryFile) error {
		now := time.Now().UTC()
		for i := range file.Entries {
			if file.Entries[i].ID == id || sameHome(file.Entries[i].NMHome, nmHome) {
				file.Entries[i].ID = id
				file.Entries[i].NMHome = nmHome
				if pid > 0 {
					file.Entries[i].PID = pid
				}
				if nmBin != "" {
					file.Entries[i].NMBin = nmBin
				}
				if ownerPID > 0 {
					file.Entries[i].OwnerPID = ownerPID
				}
				if file.Entries[i].RegisteredAt.IsZero() {
					file.Entries[i].RegisteredAt = now
				}
				return nil
			}
		}
		file.Entries = append(file.Entries, Entry{
			ID:           id,
			NMHome:       nmHome,
			PID:          pid,
			NMBin:        nmBin,
			OwnerPID:     ownerPID,
			RegisteredAt: now,
		})
		return nil
	})
}

// UpdatePID sets the recorded pid for id (or nmHome match).
func (inv *Inventory) UpdatePID(id string, pid int) error {
	if inv == nil {
		return fmt.Errorf("e2edaemon: nil inventory")
	}
	if pid <= 0 {
		return fmt.Errorf("e2edaemon: pid must be positive")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.withLock(func(file *inventoryFile) error {
		for i := range file.Entries {
			if file.Entries[i].ID == id {
				file.Entries[i].PID = pid
				return nil
			}
		}
		return fmt.Errorf("e2edaemon: entry %q not found", id)
	})
}

// Unregister removes the entry with the given id.
func (inv *Inventory) Unregister(id string) error {
	if inv == nil {
		return fmt.Errorf("e2edaemon: nil inventory")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.withLock(func(file *inventoryFile) error {
		out := file.Entries[:0]
		for _, e := range file.Entries {
			if e.ID != id {
				out = append(out, e)
			}
		}
		file.Entries = out
		return nil
	})
}

// List returns a snapshot of inventory entries.
func (inv *Inventory) List() ([]Entry, error) {
	if inv == nil {
		return nil, fmt.Errorf("e2edaemon: nil inventory")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	var entries []Entry
	err := inv.withLock(func(file *inventoryFile) error {
		entries = append([]Entry(nil), file.Entries...)
		return nil
	})
	return entries, err
}

func (inv *Inventory) withLock(fn func(*inventoryFile) error) error {
	unlock, err := lockFile(inv.lockPath())
	if err != nil {
		return err
	}
	defer unlock()

	file, err := inv.readUnlocked()
	if err != nil {
		return err
	}
	if err := fn(file); err != nil {
		return err
	}
	return inv.writeUnlocked(file)
}

func (inv *Inventory) readUnlocked() (*inventoryFile, error) {
	data, err := os.ReadFile(inv.path())
	if err != nil {
		if os.IsNotExist(err) {
			return &inventoryFile{Version: 1}, nil
		}
		return nil, fmt.Errorf("e2edaemon: read inventory: %w", err)
	}
	if len(data) == 0 {
		return &inventoryFile{Version: 1}, nil
	}
	var file inventoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		// Corrupt inventory: treat as empty so recovery can continue.
		return &inventoryFile{Version: 1}, nil
	}
	if file.Version == 0 {
		file.Version = 1
	}
	return &file, nil
}

func (inv *Inventory) writeUnlocked(file *inventoryFile) error {
	if file.Version == 0 {
		file.Version = 1
	}
	if file.Entries == nil {
		file.Entries = []Entry{}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("e2edaemon: marshal inventory: %w", err)
	}
	data = append(data, '\n')
	tmp := inv.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("e2edaemon: write inventory tmp: %w", err)
	}
	if err := os.Rename(tmp, inv.path()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("e2edaemon: rename inventory: %w", err)
	}
	return nil
}

func sameHome(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func ReapAbandoned(parent, activeDir string) []error {
	if parent == "" || activeDir == "" {
		return []error{fmt.Errorf("e2edaemon: inventory parent and active directory are required")}
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return []error{err}
	}
	activeDir, err = filepath.Abs(activeDir)
	if err != nil {
		return []error{err}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		if samePath(dir, activeDir) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ownerFileName))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			continue
		}
		alive, err := processAlive(pid)
		if err != nil || alive {
			continue
		}
		inv, err := OpenDir(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		result := inv.ReapAll()
		if len(result.Errors) > 0 {
			errs = append(errs, fmt.Errorf("%s: %s", entry.Name(), strings.Join(result.Errors, "; ")))
			continue
		}
		remaining, err := inv.List()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		if len(remaining) == 0 {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", entry.Name(), err))
			}
		}
	}
	return errs
}
