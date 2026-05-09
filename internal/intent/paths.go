package intent

import (
	"os"
	"path/filepath"
)

// resolveHome returns the home directory to use, preferring an explicit
// override (used in tests) over the OS-reported value.
func resolveHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return os.UserHomeDir()
}

// canonicalPath returns the path with symlinks evaluated and cleaned. It
// silently falls back to filepath.Clean(abs) if EvalSymlinks fails (e.g.
// the path does not exist), since both sides of the comparison receive
// the same fallback treatment.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// pathsEqual compares two paths after canonicalization.
func pathsEqual(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}
