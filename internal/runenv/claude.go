package runenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeConfigDirEnvVar selects the Claude Code profile directory. The daemon
// does not inherit it from the caller, so each run carries the caller's value.
const ClaudeConfigDirEnvVar = "CLAUDE_CONFIG_DIR"

// CallerClaudeConfigDir returns the caller's CLAUDE_CONFIG_DIR, or "" when it
// is unset. A relative value is refused, as the push hook cannot resolve one
// and a quoted "~/..." would otherwise bind a directory that does not exist.
func CallerClaudeConfigDir() (string, error) {
	dir := strings.TrimSpace(os.Getenv(ClaudeConfigDirEnvVar))
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", ClaudeConfigDirEnvVar, dir)
	}
	return filepath.Clean(dir), nil
}
