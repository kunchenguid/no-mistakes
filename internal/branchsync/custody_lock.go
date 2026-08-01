package branchsync

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var ErrCustodyLockHeld = errors.New("custody recovery is already running")

type BranchOwnershipLock struct {
	file       *os.File
	leasePath  string
	leaseToken string
	ownerPID   int
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
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = unlockCustodyLock(file)
		_ = file.Close()
		return nil, fmt.Errorf("acquire custody lock: generate lease token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	leasePath := path + ".owner"
	lease := strconv.Itoa(os.Getpid()) + " " + token + "\n"
	if err := os.WriteFile(leasePath, []byte(lease), 0o600); err != nil {
		_ = unlockCustodyLock(file)
		_ = file.Close()
		return nil, fmt.Errorf("acquire custody lock: write ownership lease: %w", err)
	}
	return &BranchOwnershipLock{file: file, leasePath: leasePath, leaseToken: token, ownerPID: os.Getpid()}, nil
}

func (l *BranchOwnershipLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	if l.leasePath != "" {
		if contents, err := os.ReadFile(l.leasePath); err == nil && strings.TrimSpace(string(contents)) == strconv.Itoa(l.ownerPID)+" "+l.leaseToken {
			_ = os.Remove(l.leasePath)
		}
	}
	_ = unlockCustodyLock(l.file)
	_ = l.file.Close()
}

type InternalMutationLockProof struct {
	Path  string
	PID   int
	Token string
}

func (l *BranchOwnershipLock) InternalMutationLockProof() InternalMutationLockProof {
	if l == nil {
		return InternalMutationLockProof{}
	}
	return InternalMutationLockProof{Path: l.leasePath, PID: l.ownerPID, Token: l.leaseToken}
}

func VerifyInternalMutationLockProof(path string, pid int, tokenHash string) bool {
	if strings.TrimSpace(path) == "" || pid <= 0 || strings.TrimSpace(tokenHash) == "" {
		return false
	}
	if !processAlive(pid) {
		return false
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 2 || fields[0] != strconv.Itoa(pid) {
		return false
	}
	sum := sha256.Sum256([]byte(fields[1]))
	return hex.EncodeToString(sum[:]) == tokenHash
}

func IssueInternalRefMutation(database *db.DB, lock *BranchOwnershipLock, spec db.InternalRefMutationSpec) (string, error) {
	if database == nil || lock == nil {
		return "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	proof := lock.InternalMutationLockProof()
	tokenSum := sha256.Sum256([]byte(proof.Token))
	if !VerifyInternalMutationLockProof(proof.Path, proof.PID, hex.EncodeToString(tokenSum[:])) {
		return "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	return database.IssueInternalRefMutation(spec, proof.Path, proof.PID, proof.Token)
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
