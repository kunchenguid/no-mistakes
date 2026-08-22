//go:build darwin || linux

package cli

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const maxOwnerPrivateKeyBytes = 4096

// readOwnerPrivateKeyFile opens the final path without following a symlink and
// validates the already-open descriptor before reading. This keeps an offline
// signer from silently consuming a key redirected through a workload-writable
// symlink or exposed to another local account.
func readOwnerPrivateKeyFile(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("open private key descriptor")
	}
	defer file.Close()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("private key must be a regular file")
	}
	if stat.Mode&0o7777 != 0o600 {
		return "", fmt.Errorf("private key mode is %04o; require 0600", stat.Mode&0o7777)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("private key is not owned by the current user")
	}
	value, err := io.ReadAll(io.LimitReader(file, maxOwnerPrivateKeyBytes+1))
	if err != nil {
		return "", err
	}
	if len(value) > maxOwnerPrivateKeyBytes {
		return "", fmt.Errorf("private key exceeds %d bytes", maxOwnerPrivateKeyBytes)
	}
	return string(value), nil
}
