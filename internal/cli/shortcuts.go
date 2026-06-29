package cli

import "github.com/spf13/cobra"

// newStShortcutCmd is the top-level human shortcut for `axi status`. It shares
// its implementation with the axi command but reports its own telemetry surface
// so human shortcut usage is measurable separately from agent driving.
func newStShortcutCmd() *cobra.Command {
	return newRunStatusCommand("st", "Show the active or most recent pipeline run in detail (shortcut for axi status)", "st", "/st")
}

// newLgShortcutCmd is the top-level human shortcut for `axi logs`, with `logs`
// as a discoverable alias.
func newLgShortcutCmd() *cobra.Command {
	return newRunLogsCommand("lg", "Show the log output of one pipeline step (shortcut for axi logs)", "lg", "/lg", []string{"logs"})
}
