package branchsync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var ErrCustodyLockHeld = errors.New("custody recovery is already running")

type BranchOwnershipLock struct {
	file                *os.File
	lockPath            string
	authorityPrefix     string
	authorityMu         sync.Mutex
	authority           *internalMutationAuthority
	authorityGeneration string
	authorityClosing    bool
	authorityDone       chan struct{}
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
	lockHash := sha256.Sum256([]byte(path))
	authorityPrefix := filepath.Join(root, ".no-mistakes-authority-"+hex.EncodeToString(lockHash[:8]))
	return &BranchOwnershipLock{file: file, lockPath: path, authorityPrefix: authorityPrefix}, nil
}

func (l *BranchOwnershipLock) Release() {
	if l == nil {
		return
	}
	authority, done := l.beginAuthorityClose()
	if authority != nil {
		authority.close()
	}
	l.authorityMu.Lock()
	if l.authorityDone == done {
		if l.file != nil {
			_ = unlockCustodyLock(l.file)
			_ = l.file.Close()
			l.file = nil
		}
		l.authorityClosing = false
		l.authorityDone = nil
		close(done)
	}
	l.authorityMu.Unlock()
}

func IssueInternalRefMutation(database *db.DB, lock *BranchOwnershipLock, spec db.InternalRefMutationSpec) (string, string, error) {
	if database == nil || lock == nil {
		return "", "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	lock.authorityMu.Lock()
	defer lock.authorityMu.Unlock()
	if lock.file == nil || lock.authorityClosing {
		return "", "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	if _, err := lock.file.Stat(); err != nil {
		return "", "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	endpoint, err := lock.ensureInternalMutationAuthorityLocked(database)
	if err != nil {
		return "", "", err
	}
	capability, err := database.IssueInternalRefMutation(spec, endpoint)
	return capability, endpoint, err
}

func (l *BranchOwnershipLock) beginAuthorityClose() (*internalMutationAuthority, chan struct{}) {
	l.authorityMu.Lock()
	for l.authorityClosing {
		done := l.authorityDone
		l.authorityMu.Unlock()
		<-done
		l.authorityMu.Lock()
	}
	l.authorityClosing = true
	done := make(chan struct{})
	l.authorityDone = done
	authority := l.authority
	l.authority = nil
	l.authorityGeneration = ""
	l.authorityMu.Unlock()
	return authority, done
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
