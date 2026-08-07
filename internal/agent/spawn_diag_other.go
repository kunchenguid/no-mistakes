//go:build !windows

package agent

// processImageName is Windows-only; the .cmd-wrapper tracking problem it
// diagnoses (issue #427) does not exist elsewhere, so this is a stub.
func processImageName(pid int) string { return "" }
