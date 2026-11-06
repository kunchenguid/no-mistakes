package daemon

import (
	"errors"
	"fmt"
	"os"
)

// restartImage is what the daemon process executes again when it stops for a
// scheduled restart: its own executable file - by then the newly installed
// version - with the arguments and environment it was started with. It is
// captured before startup adjusts the environment (login-shell PATH, unset
// agent variables), so the restarted image starts exactly as this one did.
type restartImage struct {
	executable string
	args       []string
	env        []string
}

func captureRestartImage() (restartImage, error) {
	exe, err := os.Executable()
	if err != nil {
		return restartImage{}, fmt.Errorf("resolve daemon executable: %w", err)
	}
	return restartImage{executable: exe, args: append([]string(nil), os.Args...), env: os.Environ()}, nil
}

// RunRestartable runs the daemon like Run and, when it stopped for a restart
// scheduled by `no-mistakes update` or `daemon restart --when-idle`, replaces
// this process with its executable file, keeping the process ID, so a managed
// service and anything watching the PID see one continuous daemon. Run has
// released the singleton lock, socket and database by the time it returns.
func RunRestartable() error {
	image, imageErr := captureRestartImage()
	err := Run()
	if !errors.Is(err, ErrRestartForUpdate) {
		return err
	}
	if imageErr != nil {
		return fmt.Errorf("restart for update: %w", imageErr)
	}
	return execRestartImage(image)
}
