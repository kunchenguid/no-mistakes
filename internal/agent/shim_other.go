//go:build !windows

package agent

// resolveAgentBinary is a no-op off Windows: the npm .cmd shim / cmd.exe
// argument-mangling problem it works around (issue #427) is Windows-only.
func resolveAgentBinary(bin string) string { return bin }
