package update

import (
	"fmt"
	"os"
	"os/exec"
)

func defaultSpawnBackground(currentVersion string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open null device: %w", err)
	}
	defer devNull.Close()

	cmd := exec.Command(execPath, backgroundFlag, currentVersion)
	cmd.Env = append(os.Environ(), noUpdateCheckEnv+"=1")
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = nil
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background update check: %w", err)
	}
	return nil
}
