package ipc

import "fmt"

// RequireClaudeConfigDirSupport refuses a request that carries a non-empty
// claude_config_dir when the connected daemon does not advertise support. An
// older daemon decodes requests permissively and would drop the field, so the
// run would silently use the daemon's own Claude profile instead of the
// caller's.
func (c *Client) RequireClaudeConfigDirSupport(claudeConfigDir string) error {
	if claudeConfigDir == "" {
		return nil
	}
	var health HealthResult
	if err := c.Call(MethodHealth, &HealthParams{}, &health); err != nil {
		return fmt.Errorf("check daemon support for CLAUDE_CONFIG_DIR: %w", err)
	}
	if !health.ClaudeConfigDir {
		return fmt.Errorf("the running daemon is too old to honor CLAUDE_CONFIG_DIR; restart it with `no-mistakes daemon restart` so the current binary serves it, or unset CLAUDE_CONFIG_DIR")
	}
	return nil
}
