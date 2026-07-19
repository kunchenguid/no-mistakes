//go:build !unix

package procguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Run is a transparent passthrough on platforms procguard does not shim. No
// kill/pkill/killall interposition is installed there (see Install), so this is
// only reachable via an explicit `__procguard` invocation; it simply delegates
// to the real tool if one exists.
func Run(tool string, args []string) int {
	real, err := exec.LookPath(tool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no-mistakes: %s: not available on this platform\n", tool)
		return 127
	}
	cmd := exec.Command(real, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "no-mistakes: %s: %v\n", tool, err)
		return 127
	}
	return 0
}
