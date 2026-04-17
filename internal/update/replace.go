package update

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

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
	if err := replaceExecutableAtomically(target, binaryData, perm); err == nil {
		removeQuarantine(target)
		return nil
	} else if runtime.GOOS == "darwin" {
		return fmt.Errorf("self-update requires an atomic replace on macOS; reinstall no-mistakes so the PATH entry points at a user-owned binary, then retry update: %w", err)
	}
	if err := overwriteExecutable(target, binaryData, perm); err != nil {
		return err
	}
	removeQuarantine(target)
	return nil
}

func replaceExecutableAtomically(target string, binaryData []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+"-new-*")
	if err != nil {
		return fmt.Errorf("create temp executable: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(binaryData); err != nil {
		return fmt.Errorf("write temp executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp executable: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp executable: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename temp executable: %w", err)
	}
	return nil
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
