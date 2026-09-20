// Package testgit resolves the real system git binary for tests that spawn
// a fake CLI on PATH (see internal/pipeline/fakecli). It exists because
// exec.LookPath("git") returns the PATH winner, not necessarily real git: if
// a passthrough git wrapper (guard shim, audit wrapper, etc.) sits on the
// developer's PATH, that wrapper gets mistaken for "real git" and, once the
// fake CLI directory is prepended ahead of it, the two forward into each
// other without bound (github.com/ironerumi/no-mistakes#5).
package testgit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RealGit resolves the system git binary by absolute path only, never
// consulting PATH: a wrapper (guard shim, audit wrapper, etc.) or the fake
// CLI itself can win PATH, and either one being mistaken for "real git" is
// what let fakecli and a wrapper forward into each other without bound
// (github.com/ironerumi/no-mistakes#5). Restricting resolution to a fixed
// list of well-known install locations means PATH is never consulted, so
// neither a wrapper nor the fake CLI can ever be selected.
func RealGit() (string, error) {
	locations := gitLocations()
	for _, p := range locations {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() &&
			(runtime.GOOS == "windows" || fi.Mode()&0111 != 0) {
			return p, nil
		}
	}
	return "", fmt.Errorf("testgit: real git not found in standard locations (%s)", strings.Join(locations, ", "))
}

func gitLocations() []string {
	if runtime.GOOS != "windows" {
		return []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"}
	}

	locations := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	appendRoot := func(root string) {
		if root == "" || !filepath.IsAbs(root) {
			return
		}
		for _, p := range []string{
			filepath.Join(root, "Git", "cmd", "git.exe"),
			filepath.Join(root, "Git", "bin", "git.exe"),
		} {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			locations = append(locations, p)
		}
	}

	appendRoot(os.Getenv("ProgramFiles"))
	appendRoot(`C:\Program Files`)
	appendRoot(os.Getenv("ProgramFiles(x86)"))
	appendRoot(`C:\Program Files (x86)`)
	return locations
}
