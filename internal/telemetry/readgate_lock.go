package telemetry

import (
	"fmt"
	"os"
)

type readSurfaceLock struct {
	file *os.File
}

func acquireReadSurfaceLock(path string) (*readSurfaceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open read telemetry lock: %w", err)
	}
	if err := lockReadSurfaceFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock read telemetry state: %w", err)
	}
	return &readSurfaceLock{file: file}, nil
}

func (l *readSurfaceLock) Close() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockReadSurfaceFile(l.file)
	_ = l.file.Close()
}
