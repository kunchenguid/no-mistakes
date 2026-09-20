//go:build !windows

package shellenv

// RunWindowsConsoleInterruptHelper is a no-op on non-Windows platforms.
func RunWindowsConsoleInterruptHelper(args []string) (bool, error) {
	return false, nil
}
