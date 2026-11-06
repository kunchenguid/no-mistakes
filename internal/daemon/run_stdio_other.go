//go:build !windows

package daemon

// PrepareRunStdio readies a `daemon run` process before it spawns or writes
// anything. Only Windows needs this; see run_stdio_windows.go.
func PrepareRunStdio() error {
	return nil
}
