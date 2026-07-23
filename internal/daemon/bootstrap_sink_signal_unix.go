//go:build darwin || linux

package daemon

import (
	"os"
	"os/signal"
	"syscall"
)

func protectBootstrapSink() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
}
