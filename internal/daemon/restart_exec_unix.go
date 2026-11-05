//go:build !windows

package daemon

import (
	"fmt"
	"syscall"
)

func execRestartImage(image restartImage) error {
	if err := syscall.Exec(image.executable, image.args, image.env); err != nil {
		return fmt.Errorf("restart for update: execute %s: %w", image.executable, err)
	}
	return nil
}
