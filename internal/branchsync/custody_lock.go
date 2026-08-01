package branchsync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

var ErrCustodyLockHeld = errors.New("custody recovery is already running")

type custodyLock struct {
	file *os.File
}

func acquireCustodyLock(s *Service, run *db.Run) (*custodyLock, error) {
	if s == nil || run == nil || s.Repo == nil {
		return nil, fmt.Errorf("acquire custody lock: missing recovery identity")
	}
	path := custodyLockPath(s, run)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("acquire custody lock: create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("acquire custody lock: open lock file: %w", err)
	}
	if err := tryCustodyLock(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %v", ErrCustodyLockHeld, err)
	}
	return &custodyLock{file: file}, nil
}

func (l *custodyLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockCustodyLock(l.file)
	_ = l.file.Close()
}

func custodyLockPath(s *Service, run *db.Run) string {
	root := ""
	if s.Paths != nil {
		root = s.Paths.Root()
	}
	if root == "" {
		root = s.workDir()
		if mainRoot, err := git.FindMainRepoRoot(root); err == nil {
			root = mainRoot
		}
	}
	key := sha256.Sum256([]byte(run.RepoID + "\x00" + run.ID + "\x00" + run.Branch))
	return filepath.Join(root, ".no-mistakes", "custody-locks", hex.EncodeToString(key[:])+".lock")
}
