package branchsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

const managedGateRefAuthorityMarker = "no-mistakes managed gate authority:"

type ManagedGateRefAuthority struct {
	file     *os.File
	path     string
	identity string
	marker   string
	released bool
	invalid  bool
}

func (a *ManagedGateRefAuthority) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

func (a *ManagedGateRefAuthority) Identity() string {
	if a == nil {
		return ""
	}
	return a.identity
}

func (a *ManagedGateRefAuthority) Validate(gateDir, ref string) error {
	if a == nil || a.file == nil || a.released || a.invalid || a.path == "" || !isManagedGateRef(ref) {
		return fmt.Errorf("managed gate authority is not live")
	}
	if _, err := a.file.Stat(); err != nil {
		return fmt.Errorf("managed gate authority handle is not live")
	}
	identity, err := gateRefFileIdentity(a.path)
	if err != nil || identity != a.identity {
		return fmt.Errorf("managed gate authority identity changed")
	}
	marker, err := readAuthorityMarker(a.path)
	if err != nil || marker != a.marker {
		return fmt.Errorf("managed gate authority marker changed")
	}
	if filepath.Clean(filepath.Dir(a.path)) != filepath.Clean(filepath.Join(gateDir, filepath.FromSlash(filepath.Dir(ref)))) {
		return fmt.Errorf("managed gate authority path changed")
	}
	return nil
}

func (a *ManagedGateRefAuthority) UpdateRef(ctx context.Context, gateDir, ref, oldSHA, newSHA string) error {
	if err := a.Validate(gateDir, ref); err != nil {
		return err
	}
	identity, err := gateRefFileIdentity(a.path)
	if err != nil {
		return err
	}
	if packed, err := packedGateRefExists(gateDir, ref); err != nil {
		return fmt.Errorf("inspect packed managed gate ref: %w", err)
	} else if packed {
		return fmt.Errorf("managed gate ref is packed and cannot be changed safely")
	}
	current, err := readManagedGateRef(gateDir, ref)
	if err != nil {
		return fmt.Errorf("read managed gate ref: %w", err)
	}
	expected := strings.TrimSpace(oldSHA)
	if git.IsZeroSHA(expected) || (len(expected) == 64 && expected == strings.Repeat("0", 64)) {
		expected = ""
	}
	if current != expected {
		return fmt.Errorf("managed gate ref changed from expected %s to %s", oldSHA, current)
	}
	newSHA = strings.TrimSpace(newSHA)
	if !git.IsZeroSHA(newSHA) && !(len(newSHA) == 64 && newSHA == strings.Repeat("0", 64)) {
		if _, err := git.Run(git.WithSanitizedGateConfig(ctx), gateDir, "cat-file", "-e", newSHA+"^{commit}"); err != nil {
			return fmt.Errorf("managed gate ref object is unavailable: %w", err)
		}
	}
	target := filepath.Join(gateDir, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create managed gate ref directory: %w", err)
	}
	if git.IsZeroSHA(newSHA) || (len(newSHA) == 64 && newSHA == strings.Repeat("0", 64)) {
		if current == "" {
			return nil
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove managed gate ref: %w", err)
		}
	} else {
		payloadPath := a.path + ".payload"
		payload, readErr := os.ReadFile(payloadPath)
		if readErr == nil {
			if strings.TrimSpace(string(payload)) != newSHA {
				return fmt.Errorf("managed gate authority payload conflicts with requested head")
			}
		} else if os.IsNotExist(readErr) {
			payloadFile, createErr := os.OpenFile(payloadPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if createErr != nil {
				return fmt.Errorf("stage managed gate ref: %w", createErr)
			}
			if _, writeErr := payloadFile.WriteString(newSHA + "\n"); writeErr != nil {
				_ = payloadFile.Close()
				return fmt.Errorf("write managed gate ref: %w", writeErr)
			}
			if syncErr := payloadFile.Sync(); syncErr != nil {
				_ = payloadFile.Close()
				return fmt.Errorf("sync managed gate ref: %w", syncErr)
			}
			if closeErr := payloadFile.Close(); closeErr != nil {
				return fmt.Errorf("close managed gate ref payload: %w", closeErr)
			}
		} else {
			return fmt.Errorf("inspect managed gate ref payload: %w", readErr)
		}
		if err := replaceManagedGateRef(payloadPath, target); err != nil {
			return fmt.Errorf("commit managed gate ref: %w", err)
		}
	}
	identity, err = gateRefFileIdentity(a.path)
	if err != nil || identity != a.identity {
		return fmt.Errorf("managed gate authority changed during ref transaction")
	}
	if marker, markerErr := readAuthorityMarker(a.path); markerErr != nil || marker != a.marker {
		return fmt.Errorf("managed gate authority changed during ref transaction")
	}
	return nil
}

func ReadManagedGateRefUnderAuthority(a *ManagedGateRefAuthority, gateDir, ref string) (string, error) {
	if err := a.Validate(gateDir, ref); err != nil {
		return "", err
	}
	return readManagedGateRef(gateDir, ref)
}

func AcquireManagedGateRefAuthority(gateDir, ref string) (*ManagedGateRefAuthority, error) {
	if strings.TrimSpace(gateDir) == "" || !strings.HasPrefix(ref, "refs/heads/") || !isManagedGateRef(ref) {
		return nil, fmt.Errorf("managed gate authority requires an ordinary branch ref")
	}
	return acquireManagedRefAuthority(gateDir, ref)
}

func AcquireManagedPrivateRefAuthority(gitDir, ref string) (*ManagedGateRefAuthority, error) {
	if strings.TrimSpace(gitDir) == "" || !strings.HasPrefix(ref, "refs/no-mistakes/") || !isManagedGateRef(ref) {
		return nil, fmt.Errorf("managed private ref authority requires a no-mistakes ref")
	}
	return acquireManagedRefAuthority(gitDir, ref)
}

func acquireManagedRefAuthority(gateDir, ref string) (*ManagedGateRefAuthority, error) {
	path := filepath.Join(gateDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create managed gate authority directory: %w", err)
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate managed gate authority token: %w", err)
	}
	markerValue := fmt.Sprintf("%s %d %s", managedGateRefAuthorityMarker, os.Getpid(), hex.EncodeToString(tokenBytes))
	marker := []byte(markerValue + "\n")
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
			return &ManagedGateRefAuthority{file: file, path: path, identity: identity, marker: markerValue}, nil
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

func (a *ManagedGateRefAuthority) Invalidate() error {
	if a == nil || a.released {
		return nil
	}
	if a.file != nil {
		releaseGateRefOSLock(a.file)
		if err := a.file.Close(); err != nil {
			a.file = nil
			a.released = true
			a.invalid = true
			return fmt.Errorf("close invalid managed gate authority: %w", err)
		}
		a.file = nil
	}
	a.released = true
	a.invalid = true
	return nil
}

func readAuthorityMarker(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	value, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(value), "\n"), nil
}
