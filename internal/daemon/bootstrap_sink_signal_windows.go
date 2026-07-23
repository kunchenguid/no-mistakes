package daemon

import (
	"os"
	"os/signal"
)

func protectBootstrapSink() {
	signal.Ignore(os.Interrupt)
}
