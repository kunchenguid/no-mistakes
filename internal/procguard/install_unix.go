//go:build unix

package procguard

import (
	"fmt"
	"os"
	"path/filepath"
)

// shimNames are the tool names procguard interposes on the agent PATH.
var shimNames = []string{"kill", "pkill", "killall"}

// Install creates (or refreshes) the interposition shim directory under the
// given NM_HOME root: BinDir(root) containing kill/pkill/killall symlinks to the
// running no-mistakes executable. Dispatch recognizes those names via argv[0]
// and runs the guard.
//
// It is idempotent and safe to call on every daemon startup, which is important
// because os.Executable() changes after a `no-mistakes update`; reinstalling
// repoints the symlinks at the current binary. A non-nil error means the guard
// could not be installed and callers should treat the interposition layer as
// inactive (the daemon logs a warning and continues; the OS-native layer, not
// this best-effort PATH layer, is the adversarial-grade control).
func Install(root string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := BinDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for _, name := range shimNames {
		link := filepath.Join(dir, name)
		if cur, err := os.Readlink(link); err == nil && cur == exe {
			continue // already correct
		}
		// Replace whatever is there (stale symlink, or nothing) atomically enough
		// for a start-time install: remove then recreate.
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale shim %s: %w", link, err)
		}
		if err := os.Symlink(exe, link); err != nil {
			return fmt.Errorf("link shim %s: %w", link, err)
		}
	}
	return nil
}
