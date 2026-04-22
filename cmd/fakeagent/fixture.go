package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// fixtureDir returns the agent's fixture directory if FAKEAGENT_FIXTURE
// is set and contains a subdirectory for this agent, e.g.
// FAKEAGENT_FIXTURE=internal/e2e/fixtures + agent=claude →
// internal/e2e/fixtures/claude. Returns "" if no fixture is configured.
func fixtureDir(agent string) string {
	root := os.Getenv("FAKEAGENT_FIXTURE")
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, agent)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// readFixtureFile reads a file from the fixture directory. The flavour
// arg picks between recorded variants ("structured" vs "plain"). Returns
// (nil, nil) when no fixture exists, so callers can decide whether to
// fall back to synthetic generation.
func readFixtureFile(dir, flavour, name string) ([]byte, error) {
	if dir == "" {
		return nil, nil
	}
	// Try <dir>/<flavour>/<name> first (opencode layout), then
	// <dir>/<flavour>.<ext> (claude/codex layout where flavour is
	// the file basename and name carries the extension).
	candidates := []string{
		filepath.Join(dir, flavour, name),
		filepath.Join(dir, flavour+filepath.Ext(name)),
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read fixture %s: %w", p, err)
		}
	}
	return nil, nil
}
