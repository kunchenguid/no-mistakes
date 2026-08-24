//go:build windows

package cli

import "fmt"

// Windows FileMode bits do not prove an owner-only ACL. Fail closed until the
// CLI has a native ACL verifier rather than pretending 0600 is authoritative.
func readOwnerPrivateKeyFile(path string) (string, error) {
	return "", fmt.Errorf("secure owner-decision private-key loading is not supported on Windows")
}
