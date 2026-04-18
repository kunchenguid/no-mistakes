package cli

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func trackCommand(name string, fn func() error) (err error) {
	start := time.Now()
	err = fn()
	telemetry.Track("command", telemetry.Fields{
		"command":     name,
		"status":      commandStatus(err),
		"duration_ms": time.Since(start).Milliseconds(),
	})
	return err
}

func commandStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
