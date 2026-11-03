//go:build windows

package daemon

import "errors"

func execRestartImage(restartImage) error {
	return errors.New("restart for update: replacing the daemon process is not supported on Windows")
}
