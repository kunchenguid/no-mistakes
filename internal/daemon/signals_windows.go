//go:build windows

package daemon

import "os"

func daemonSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
