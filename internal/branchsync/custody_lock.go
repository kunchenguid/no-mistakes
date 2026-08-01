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
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var ErrCustodyLockHeld = errors.New("custody recovery is already running")

type BranchOwnershipLock struct {
	file *os.File
}

type custodyLock = BranchOwnershipLock

func AcquireBranchOwnershipLock(p *paths.Paths, repo *db.Repo, workDir, branch string) (*BranchOwnershipLock, error) {
	if repo == nil {
		return nil, fmt.Errorf("acquire branch ownership lock: missing repository")
	}
	root := ""
	if p != nil {
		root = p.Root()
	}
	if root == "" {
		root = workDir
		if mainRoot, err := git.FindMainRepoRoot(root); err == nil {
			root = mainRoot
		}
	}
	return acquireBranchOwnershipLock(root, repo.WorkingPath, workDir, branch)
}

func acquireCustodyLock(s *Service, run *db.Run) (*custodyLock, error) {
	if s == nil || run == nil || s.Repo == nil {
		return nil, fmt.Errorf("acquire custody lock: missing recovery identity")
	}
	return AcquireBranchOwnershipLock(s.Paths, s.Repo, s.workDir(), run.Branch)
}

func acquireBranchOwnershipLock(root, repository, workDir, branch string) (*BranchOwnershipLock, error) {
	path := branchOwnershipLockPath(root, repository, workDir, branch)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("acquire custody lock: create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("acquire custody lock: open lock file: %w", err)
	}
	if err := tryCustodyLock(file); err != nil {
		_ = file.Close()
		if isCustodyLockContention(err) {
			return nil, fmt.Errorf("%w: %v", ErrCustodyLockHeld, err)
		}
		return nil, fmt.Errorf("acquire custody lock: lock file: %w", err)
	}
	return &BranchOwnershipLock{file: file}, nil
}

func (l *BranchOwnershipLock) Release() {
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
	repository := s.Repo.WorkingPath
	if repository == "" {
		repository = s.workDir()
		if mainRoot, err := git.FindMainRepoRoot(repository); err == nil {
			repository = mainRoot
		}
	}
	if absolute, err := filepath.Abs(repository); err == nil {
		repository = absolute
	}
	if resolved, err := filepath.EvalSymlinks(repository); err == nil {
		repository = resolved
	}
	return branchOwnershipLockPath(root, repository, s.workDir(), run.Branch)
}

func branchOwnershipLockPath(root, repository, workDir, branch string) string {
	if repository == "" {
		repository = workDir
		if mainRoot, err := git.FindMainRepoRoot(repository); err == nil {
			repository = mainRoot
		}
	}
	if absolute, err := filepath.Abs(repository); err == nil {
		repository = absolute
	}
	if resolved, err := filepath.EvalSymlinks(repository); err == nil {
		repository = resolved
	}
	key := sha256.Sum256([]byte(filepath.Clean(repository) + "\x00" + branch))
	return filepath.Join(root, ".no-mistakes", "custody-locks", hex.EncodeToString(key[:])+".lock")
}
