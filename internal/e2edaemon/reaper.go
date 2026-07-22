package e2edaemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ReapResult summarizes one reaper pass.
type ReapResult struct {
	Entries int
	Stopped int
	Killed  int
	Removed int
	Skipped int
	Errors  []string
}

// ReapAll stops every inventoried temporary daemon with bounded ownership
// checks, then removes their inventory entries. It never touches processes
// that are not listed in the inventory or whose argv no longer matches the
// recorded NM_HOME (so the shared service is never a target).
func (inv *Inventory) ReapAll() ReapResult {
	var result ReapResult
	if inv == nil {
		result.Errors = append(result.Errors, "nil inventory")
		return result
	}
	entries, err := inv.List()
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	result.Entries = len(entries)
	for _, e := range entries {
		if err := inv.reapEntry(e, &result); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", e.ID, err))
		}
	}
	return result
}

func (inv *Inventory) reapEntry(e Entry, result *ReapResult) error {
	if !isAllowedTempRoot(e.NMHome) {
		result.Skipped++
		// Drop the unsafe inventory row so it cannot be retried forever,
		// but do not signal or stop anything.
		_ = inv.Unregister(e.ID)
		result.Removed++
		return fmt.Errorf("refusing to reap non-temp root %q", e.NMHome)
	}

	// Prefer graceful stop via the recorded binary when available.
	if stopped := tryDaemonStop(e); stopped {
		result.Stopped++
	}

	// Kill only PIDs that still match daemon run --root <exact home>.
	targets := map[int]struct{}{}
	if e.PID > 0 && MatchesDaemonRoot(e.PID, e.NMHome) {
		targets[e.PID] = struct{}{}
	}
	if found, err := FindDaemonsForRoot(e.NMHome); err == nil {
		for _, pid := range found {
			targets[pid] = struct{}{}
		}
	}
	for pid := range targets {
		if !MatchesDaemonRoot(pid, e.NMHome) {
			continue
		}
		if err := terminateMatched(pid); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("kill %d: %v", pid, err))
			continue
		}
		result.Killed++
	}

	if err := inv.Unregister(e.ID); err != nil {
		return err
	}
	result.Removed++
	return nil
}

// isAllowedTempRoot is a hard safety gate: never operate on the shared
// user NM_HOME or other non-temp paths even if inventory is corrupted.
func isAllowedTempRoot(nmHome string) bool {
	nmHome = filepath.Clean(nmHome)
	if nmHome == "" || nmHome == "." || nmHome == string(filepath.Separator) {
		return false
	}
	// Reject the default shared root spellings.
	if home, err := os.UserHomeDir(); err == nil {
		shared := filepath.Join(home, ".no-mistakes")
		if samePath(nmHome, shared) {
			return false
		}
		if resolved, err := filepath.EvalSymlinks(shared); err == nil && samePath(nmHome, resolved) {
			return false
		}
	}
	// Require a temp-looking path segment. Harness uses nm-e2e-*; daemon_run
	// tests use nmh-*; suite labs use nm-e2e-keepalive / private/tmp prefixes.
	lower := strings.ToLower(nmHome)
	if strings.Contains(lower, string(filepath.Separator)+"nm-e2e") ||
		strings.Contains(lower, string(filepath.Separator)+"nmh-") ||
		strings.Contains(lower, "/tmp/") ||
		strings.Contains(lower, "/private/tmp/") ||
		strings.Contains(lower, string(filepath.Separator)+"tmp"+string(filepath.Separator)) {
		return true
	}
	// Windows temp
	if strings.Contains(lower, `\temp\`) || strings.Contains(lower, `/temp/`) {
		return true
	}
	return false
}

func tryDaemonStop(e Entry) bool {
	bin := e.NMBin
	if bin == "" {
		return false
	}
	if _, err := os.Stat(bin); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon", "stop")
	cmd.Env = append(os.Environ(),
		"NM_HOME="+e.NMHome,
		"NM_TEST_START_DAEMON=1",
		"NO_MISTAKES_TELEMETRY=off",
		"NO_MISTAKES_NO_UPDATE_CHECK=1",
	)
	cmd.Dir = os.TempDir()
	_ = cmd.Run()
	// Consider stop successful when no matching process remains.
	if e.PID > 0 {
		alive, _ := processAlive(e.PID)
		if !alive || !MatchesDaemonRoot(e.PID, e.NMHome) {
			return true
		}
	}
	found, _ := FindDaemonsForRoot(e.NMHome)
	return len(found) == 0
}

func terminateMatched(pid int) error {
	// Re-verify the process is still a daemon run before signalling.
	cmd, err := processCommandLine(pid)
	if err != nil {
		return err
	}
	if !looksLikeDaemonRun(cmd) {
		return fmt.Errorf("pid %d no longer a daemon run", pid)
	}
	return terminateDaemonPID(pid)
}

// Ownership tracks one harness's inventory entry and concurrency slot.
type Ownership struct {
	Inv    *Inventory
	ID     string
	NMHome string
	NMBin  string
	Slot   *Slot
}

// Acquire creates inventory ownership for a temp NM_HOME under the suite
// concurrency cap. Call SyncPID after the daemon becomes live; call Release
// after stop (or from the reaper path).
func Acquire(nmHome, nmBin string, slotTimeout time.Duration) (*Ownership, error) {
	inv, err := Open()
	if err != nil {
		return nil, err
	}
	if !isAllowedTempRoot(nmHome) {
		return nil, fmt.Errorf("e2edaemon: nm_home %q is not an allowed temp root", nmHome)
	}
	slot, err := inv.AcquireSlot(slotTimeout)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	own := &Ownership{
		Inv:    inv,
		ID:     id,
		NMHome: filepath.Clean(nmHome),
		NMBin:  nmBin,
		Slot:   slot,
	}
	if err := inv.Register(id, own.NMHome, nmBin, 0, os.Getpid()); err != nil {
		slot.Release()
		return nil, err
	}
	return own, nil
}

// SyncPID reads a live daemon pid (from caller) into the inventory.
func (o *Ownership) SyncPID(pid int) error {
	if o == nil || o.Inv == nil {
		return fmt.Errorf("e2edaemon: nil ownership")
	}
	if pid <= 0 {
		return nil
	}
	return o.Inv.UpdatePID(o.ID, pid)
}

// StopBestEffort runs daemon stop for this home when a binary is known.
func (o *Ownership) StopBestEffort() {
	if o == nil {
		return
	}
	_ = tryDaemonStop(Entry{
		ID:     o.ID,
		NMHome: o.NMHome,
		NMBin:  o.NMBin,
	})
}

// Release stops best-effort, reaps any leftover matched process for this
// home only, unregisters the inventory entry, and frees the slot.
func (o *Ownership) Release() {
	if o == nil {
		return
	}
	o.StopBestEffort()
	if o.Inv != nil {
		// Targeted kill for this home only.
		if found, err := FindDaemonsForRoot(o.NMHome); err == nil {
			for _, pid := range found {
				if MatchesDaemonRoot(pid, o.NMHome) {
					_ = terminateMatched(pid)
				}
			}
		}
		_ = o.Inv.Unregister(o.ID)
	}
	if o.Slot != nil {
		o.Slot.Release()
	}
}
