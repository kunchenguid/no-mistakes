//go:build windows

package daemon

import (
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// execRestartImage starts the restarted daemon on Windows, which cannot
// replace a running process image: the stopping daemon launches its
// executable file - by then the new version - as a detached daemon with the
// arguments and environment it was started with, and exits once that daemon
// answers. The singleton lock, socket and database were released when Run
// returned, so the new daemon takes them over. Unlike the in-place restart
// on macOS and Linux, the process ID changes.
func execRestartImage(image restartImage) error {
	p, err := paths.New()
	if err != nil {
		return fmt.Errorf("restart for update: resolve paths: %w", err)
	}
	var args []string
	if len(image.args) > 1 {
		args = image.args[1:]
	}
	cmd := exec.Command(image.executable, args...)
	cmd.Env = image.env
	if err := launchDetachedDaemon(p, cmd); err != nil {
		return fmt.Errorf("restart for update: start %s: %w", image.executable, err)
	}
	return nil
}
