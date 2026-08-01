package branchsync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const gateRefLockMarker = "no-mistakes gate lock authority:"

type gateRefLock struct {
	file *os.File
	path string
}

func acquireGateRefLock(gateDir, ref, authorityEndpoint string) (*gateRefLock, error) {
	if strings.TrimSpace(gateDir) == "" || !strings.HasPrefix(ref, "refs/heads/") || strings.TrimSpace(authorityEndpoint) == "" {
		return nil, fmt.Errorf("ordinary gate ref lock requires a managed branch ref")
	}
	path := filepath.Join(gateDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create ordinary gate ref lock directory: %w", err)
	}
	marker := []byte(gateRefLockMarker + " " + strings.TrimSpace(authorityEndpoint) + "\n")
	for attempt := 0; attempt < 2; attempt++ {
		file, err := createGateRefLock(path, marker)
		if err == nil {
			return &gateRefLock{file: file, path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire ordinary gate ref lock: %w", err)
		}
		stale, staleErr := staleGateRefLock(path)
		if staleErr != nil {
			return nil, fmt.Errorf("acquire ordinary gate ref lock: %w", staleErr)
		}
		if !stale {
			return nil, fmt.Errorf("acquire ordinary gate ref lock: another Git transaction owns %s", ref)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale ordinary gate ref lock: %w", err)
		}
	}
	return nil, fmt.Errorf("acquire ordinary gate ref lock: stale lock contention for %s", ref)
}

func createGateRefLock(path string, marker []byte) (*os.File, error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".no-mistakes-gate-lock-*")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(marker); err != nil {
		_ = temp.Close()
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tempPath, path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func staleGateRefLock(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	marker, err := io.ReadAll(file)
	if err != nil {
		return false, err
	}
	value := strings.TrimSpace(string(marker))
	prefix := gateRefLockMarker + " "
	if !strings.HasPrefix(value, prefix) {
		return false, fmt.Errorf("ordinary gate ref lock is not owned by no-mistakes")
	}
	endpoint := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if endpoint == "" {
		return false, fmt.Errorf("ordinary gate ref lock has no owner authority")
	}
	conn, err := dialInternalMutationAuthority(endpoint)
	if err == nil {
		_ = conn.Close()
		return false, nil
	}
	return true, nil
}

func readLockedGateRef(gateDir, ref string) (string, error) {
	return readLockedGateRefAtDepth(gateDir, ref, 0)
}

func readLockedGateRefAtDepth(gateDir, ref string, depth int) (string, error) {
	if depth > 8 || strings.TrimSpace(gateDir) == "" || !strings.HasPrefix(ref, "refs/") {
		return "", fmt.Errorf("read locked gate ref: invalid ref")
	}
	loose, err := os.ReadFile(filepath.Join(gateDir, filepath.FromSlash(ref)))
	if err == nil {
		value := strings.TrimSpace(string(loose))
		if strings.HasPrefix(value, "ref: ") {
			return readLockedGateRefAtDepth(gateDir, strings.TrimSpace(strings.TrimPrefix(value, "ref: ")), depth+1)
		}
		if value == "" || strings.ContainsAny(value, " \t\r\n") {
			return "", fmt.Errorf("read locked gate ref: malformed loose ref %s", ref)
		}
		return value, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	packed, err := os.ReadFile(filepath.Join(gateDir, "packed-refs"))
	if err != nil {
		return "", fmt.Errorf("read locked gate ref: %s is absent", ref)
	}
	for _, line := range strings.Split(string(packed), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("read locked gate ref: %s is absent", ref)
}

func (l *gateRefLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
}
