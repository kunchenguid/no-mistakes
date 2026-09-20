//go:build !windows

package shellenv

// RunWindowsCooperativeCommandHelper is a no-op on non-Windows platforms.
func RunWindowsCooperativeCommandHelper(args []string) (bool, int, error) {
	return false, 0, nil
}
