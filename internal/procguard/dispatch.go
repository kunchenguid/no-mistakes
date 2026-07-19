package procguard

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dispatch decides whether this process was launched as a guard shim and, if so,
// runs the guard and returns its exit code. It must be called at the very top of
// main so an invocation via the kill/pkill/killall symlinks (or the explicit
// `__procguard <tool>` subcommand) is handled before any normal CLI/daemon
// startup work.
//
// It returns (code, true) when it handled the invocation, or (0, false) when the
// process should continue with its ordinary entrypoint.
func Dispatch(argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	var tool string
	var rest []string
	switch {
	case len(argv) >= 2 && argv[1] == "__procguard":
		if len(argv) < 3 {
			fmt.Fprintln(os.Stderr, "no-mistakes __procguard: missing tool name")
			return exitUnsupportedDispatch, true
		}
		tool = filepath.Base(argv[2])
		rest = argv[3:]
	case IsGuardTool(argv[0]):
		tool = filepath.Base(argv[0])
		rest = argv[1:]
	default:
		return 0, false
	}
	return Run(tool, rest), true
}

// exitUnsupportedDispatch mirrors the unsupported exit code without importing
// the unix-only constant on every platform.
const exitUnsupportedDispatch = 4
