package branchsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const managedGateRefAuthorityMarker = "no-mistakes managed gate authority:"

type ManagedGateRefAuthority struct {
	file     *os.File
	path     string
	identity string
	released bool
}

func AcquireManagedGateRefAuthority(gateDir, ref string) (*ManagedGateRefAuthority, error) {
	if strings.TrimSpace(gateDir) == "" || !strings.HasPrefix(ref, "refs/heads/") || !isManagedGateRef(ref) {
		return nil, fmt.Errorf("managed gate authority requires an ordinary branch ref")
	}
	path := filepath.Join(gateDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create managed gate authority directory: %w", err)
	}
	marker := []byte(fmt.Sprintf("%s %d\n", managedGateRefAuthorityMarker, os.Getpid()))
	for attempt := 0; attempt < 2; attempt++ {
		file, err := createGateRefLock(path, marker)
		if err == nil {
			if lockErr := acquireGateRefOSLock(file); lockErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("acquire managed gate authority: %w", lockErr)
			}
			identity, identityErr := gateRefFileIdentity(path)
			if identityErr != nil {
				releaseGateRefOSLock(file)
				_ = file.Close()
				_ = os.Remove(path)
				return nil, identityErr
			}
			return &ManagedGateRefAuthority{file: file, path: path, identity: identity}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire managed gate authority: %w", err)
		}
		existing, openErr := os.OpenFile(path, os.O_RDWR, 0o644)
		if openErr != nil {
			return nil, fmt.Errorf("inspect managed gate authority: %w", openErr)
		}
		markerBytes, readErr := os.ReadFile(path)
		if readErr != nil {
			_ = existing.Close()
			return nil, fmt.Errorf("read managed gate authority: %w", readErr)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(markerBytes)), managedGateRefAuthorityMarker+" ") {
			_ = existing.Close()
			return nil, fmt.Errorf("managed gate ref lock is owned by another writer")
		}
		if lockErr := acquireGateRefOSLock(existing); lockErr != nil {
			_ = existing.Close()
			return nil, fmt.Errorf("managed gate authority is still live: %w", lockErr)
		}
		releaseGateRefOSLock(existing)
		_ = existing.Close()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale managed gate authority: %w", err)
		}
	}
	return nil, fmt.Errorf("managed gate authority contention for %s", ref)
}

func (a *ManagedGateRefAuthority) Release() error {
	if a == nil || a.released {
		return nil
	}
	if a.file != nil {
		releaseGateRefOSLock(a.file)
		if err := a.file.Close(); err != nil {
			return fmt.Errorf("close managed gate authority: %w", err)
		}
		a.file = nil
	}
	if identity, err := gateRefFileIdentity(a.path); err == nil && identity == a.identity {
		if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove managed gate authority: %w", err)
		}
	}
	a.released = true
	return nil
}
