package branchsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func readManagedGateRef(gateDir, ref string) (string, error) {
	value, err := readLockedGateRef(gateDir, ref)
	if errors.Is(err, errGateRefAbsent) {
		return "", nil
	}
	return value, err
}

func packedGateRefExists(gateDir, ref string) (bool, error) {
	packed, err := os.ReadFile(filepath.Join(gateDir, "packed-refs"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return true, nil
		}
	}
	return false, nil
}

func (l *gateRefLock) commitRef(ctx context.Context, gateDir, ref, oldSHA, newSHA string) error {
	if l == nil || l.file == nil || l.path == "" || !isManagedGateRef(ref) {
		return fmt.Errorf("managed gate ref transaction requires an active lock")
	}
	if err := acquireGateRefOSLock(l.file); err != nil {
		return fmt.Errorf("acquire managed gate ref transaction: %w", err)
	}
	l.osLocked = true
	packed, err := packedGateRefExists(gateDir, ref)
	if err != nil {
		return fmt.Errorf("inspect packed managed gate ref: %w", err)
	}
	if packed {
		return fmt.Errorf("managed gate ref is packed and cannot be changed safely")
	}
	current, err := readManagedGateRef(gateDir, ref)
	if err != nil {
		return fmt.Errorf("read managed gate ref: %w", err)
	}
	if current != oldSHA {
		return fmt.Errorf("managed gate ref changed from expected %s to %s", oldSHA, current)
	}
	newSHA = strings.TrimSpace(newSHA)
	if !git.IsZeroSHA(newSHA) && !(len(newSHA) == 64 && newSHA == strings.Repeat("0", 64)) {
		checkCtx := git.WithSanitizedGateConfig(ctx)
		if _, err := git.Run(checkCtx, gateDir, "cat-file", "-e", newSHA+"^{commit}"); err != nil {
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
		return nil
	}
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("stage managed gate ref: %w", err)
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind managed gate ref lock: %w", err)
	}
	if _, err := l.file.WriteString(newSHA + "\n"); err != nil {
		return fmt.Errorf("write managed gate ref: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync managed gate ref: %w", err)
	}
	if runtime.GOOS == "windows" {
		releaseGateRefOSLock(l.file)
		l.osLocked = false
		if err := l.file.Close(); err != nil {
			l.file = nil
			return fmt.Errorf("close managed gate ref lock: %w", err)
		}
		l.file = nil
	}
	if err := replaceManagedGateRef(l.path, target); err != nil {
		return fmt.Errorf("commit managed gate ref: %w", err)
	}
	return nil
}
