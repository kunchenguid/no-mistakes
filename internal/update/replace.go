package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var renameFile = os.Rename
var removeFile = os.Remove

func replaceExecutable(target string, binaryData []byte) error {
	resolved, err := filepath.EvalSymlinks(target)
	if err == nil {
		target = resolved
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	perm := info.Mode().Perm()
	if currentGOOS == "windows" {
		return replaceExecutableOnWindows(target, binaryData, perm)
	}
	if err := replaceExecutableAtomically(target, binaryData, perm); err == nil {
		removeQuarantine(target)
		return nil
	} else if currentGOOS == "darwin" {
		return fmt.Errorf("self-update requires an atomic replace on macOS; reinstall no-mistakes so the PATH entry points at a user-owned binary, then retry update: %w", err)
	}
	if err := overwriteExecutable(target, binaryData, perm); err != nil {
		return err
	}
	removeQuarantine(target)
	return nil
}

func replaceExecutableOnWindows(target string, binaryData []byte, perm os.FileMode) error {
	tmpPath, cleanup, err := stageExecutable(target, binaryData, perm)
	if err != nil {
		return err
	}
	defer cleanup()

	backupPath := target + ".old"
	if err := removeFile(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale executable backup: %w", err)
	}
	if err := renameFile(target, backupPath); err != nil {
		return fmt.Errorf("move running executable to backup: %w", err)
	}
	if err := renameFile(tmpPath, target); err != nil {
		installErr := fmt.Errorf("install staged executable: %w", err)
		if restoreErr := renameFile(backupPath, target); restoreErr != nil {
			return errors.Join(installErr, fmt.Errorf("restore previous executable: %w", restoreErr))
		}
		return installErr
	}
	return nil
}

// CleanupOldExecutable removes the backup left by a successful Windows update.
// The running process no longer holds that image open on the next startup.
func CleanupOldExecutable() error {
	if currentGOOS != "windows" {
		return nil
	}
	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable for update cleanup: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return cleanupOldExecutable(target)
}

func cleanupOldExecutable(target string) error {
	if err := removeFile(target + ".old"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old executable backup: %w", err)
	}
	return nil
}

func replaceExecutableAtomically(target string, binaryData []byte, perm os.FileMode) error {
	tmpPath, cleanup, err := stageExecutable(target, binaryData, perm)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := renameFile(tmpPath, target); err != nil {
		return fmt.Errorf("rename temp executable: %w", err)
	}
	return nil
}

func stageExecutable(target string, binaryData []byte, perm os.FileMode) (string, func(), error) {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+"-new-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp executable: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(binaryData); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write temp executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp executable: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chmod temp executable: %w", err)
	}
	return tmpPath, cleanup, nil
}

func overwriteExecutable(path string, binaryData []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("overwrite executable: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(binaryData); err != nil {
		return fmt.Errorf("overwrite executable: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod executable: %w", err)
	}
	return nil
}
