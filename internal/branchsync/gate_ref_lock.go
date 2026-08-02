package branchsync

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const gateRefLockMarker = "no-mistakes gate lock authority:"
const gateRefLockOwnerMarker = "no-mistakes gate lock owner:"

var removeGateRefLock = os.Remove

var errGateRefAbsent = errors.New("gate ref is absent")

type gateRefLockOwner struct {
	RunID             string `json:"run_id"`
	RepoID            string `json:"repo_id"`
	GatePath          string `json:"gate_path"`
	Branch            string `json:"branch"`
	Ref               string `json:"ref"`
	OwnerGeneration   string `json:"owner_generation"`
	AuthorityEndpoint string `json:"authority_endpoint"`
	ExpectedHead      string `json:"expected_head"`
}

type gateRefLock struct {
	file        *os.File
	path        string
	payloadPath string
	owner       gateRefLockOwner
	identity    string
	database    *db.DB
	osLocked    bool
	commitDone  bool
	released    bool
	releaseErr  error
}

func acquireGateRefLock(gateDir, ref, authorityEndpoint string) (*gateRefLock, error) {
	if strings.TrimSpace(gateDir) == "" || !isManagedGateRef(ref) || strings.TrimSpace(authorityEndpoint) == "" {
		return nil, fmt.Errorf("managed gate ref lock requires a managed ref")
	}
	path := filepath.Join(gateDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create managed gate ref lock directory: %w", err)
	}
	marker := []byte(gateRefLockMarker + " " + strings.TrimSpace(authorityEndpoint) + "\n")
	for attempt := 0; attempt < 2; attempt++ {
		file, err := createGateRefLock(path, marker)
		if err == nil {
			identity, identityErr := gateRefFileIdentity(path)
			if identityErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("identify managed gate ref lock: %w", identityErr)
			}
			return &gateRefLock{file: file, path: path, identity: identity}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire managed gate ref lock: %w", err)
		}
		stale, staleErr := staleGateRefLock(path)
		if staleErr != nil {
			return nil, fmt.Errorf("acquire managed gate ref lock: %w", staleErr)
		}
		if !stale {
			return nil, fmt.Errorf("acquire managed gate ref lock: another Git transaction owns %s", ref)
		}
		if err := os.Remove(path + ".payload"); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale managed gate ref payload: %w", err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale managed gate ref lock: %w", err)
		}
	}
	return nil, fmt.Errorf("acquire managed gate ref lock: stale lock contention for %s", ref)
}

func isManagedGateRef(ref string) bool {
	raw := ref
	ref = strings.TrimSpace(ref)
	return ref != "" && raw == ref && !strings.Contains(ref, "\\") && !strings.Contains(ref, "//") && !strings.Contains(ref, "/./") && !strings.Contains(ref, "/../") && !strings.HasSuffix(ref, "/") && (strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/no-mistakes/"))
}

func newGateRefLockGeneration() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func acquireOwnedGateRefLock(gateDir, ref string, owner gateRefLockOwner) (*gateRefLock, error) {
	if strings.TrimSpace(gateDir) == "" || !strings.HasPrefix(ref, "refs/heads/") || strings.TrimSpace(owner.RunID) == "" || strings.TrimSpace(owner.RepoID) == "" || strings.TrimSpace(owner.GatePath) == "" || strings.TrimSpace(owner.Branch) == "" || strings.TrimSpace(owner.OwnerGeneration) == "" || strings.TrimSpace(owner.AuthorityEndpoint) == "" || strings.TrimSpace(owner.ExpectedHead) == "" || owner.Ref != ref {
		return nil, fmt.Errorf("owned ordinary gate ref lock requires exact ownership metadata")
	}
	path := filepath.Join(gateDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create ordinary gate ref lock directory: %w", err)
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		return nil, fmt.Errorf("encode ordinary gate ref lock owner: %w", err)
	}
	marker := append([]byte(gateRefLockOwnerMarker+" "), encoded...)
	marker = append(marker, '\n')
	for attempt := 0; attempt < 2; attempt++ {
		file, err := createGateRefLock(path, marker)
		if err == nil {
			identity, identityErr := gateRefFileIdentity(path)
			if identityErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("identify ordinary gate ref lock: %w", identityErr)
			}
			return &gateRefLock{file: file, path: path, owner: owner, identity: identity}, nil
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
		if err := os.Remove(path + ".payload"); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale ordinary gate ref payload: %w", err)
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
	if strings.HasPrefix(value, gateRefLockOwnerMarker+" ") {
		var owner gateRefLockOwner
		if err := json.Unmarshal([]byte(strings.TrimPrefix(value, gateRefLockOwnerMarker+" ")), &owner); err != nil {
			return false, fmt.Errorf("ordinary gate ref lock owner is malformed")
		}
		if strings.TrimSpace(owner.AuthorityEndpoint) == "" || strings.TrimSpace(owner.OwnerGeneration) == "" || strings.TrimSpace(owner.Ref) == "" {
			return false, fmt.Errorf("ordinary gate ref lock owner is incomplete")
		}
		conn, err := dialInternalMutationAuthority(owner.AuthorityEndpoint)
		if err == nil {
			_ = conn.Close()
			return false, nil
		}
		return true, nil
	}
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

func gateRefFileIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("ordinary gate ref lock is a directory")
	}
	return gateRefFileIdentityValue(info), nil
}

func readOwnedGateRefLock(path string) (gateRefLockOwner, error) {
	file, err := os.Open(path)
	if err != nil {
		return gateRefLockOwner{}, err
	}
	defer file.Close()
	marker, err := io.ReadAll(file)
	if err != nil {
		return gateRefLockOwner{}, err
	}
	prefix := gateRefLockOwnerMarker + " "
	value := strings.TrimSpace(string(marker))
	if !strings.HasPrefix(value, prefix) {
		return gateRefLockOwner{}, fmt.Errorf("ordinary gate ref lock has no exact owner")
	}
	var owner gateRefLockOwner
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, prefix)), &owner); err != nil {
		return gateRefLockOwner{}, fmt.Errorf("ordinary gate ref lock owner is malformed")
	}
	return owner, nil
}

func (l *gateRefLock) setOwner(owner gateRefLockOwner) error {
	if l == nil || l.file == nil {
		return fmt.Errorf("managed gate ref lock handle is unavailable")
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.Write(append([]byte(gateRefLockOwnerMarker+" "), append(encoded, '\n')...)); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *gateRefLock) release(database *db.DB) error {
	if l == nil {
		return nil
	}
	if l.released {
		return l.releaseErr
	}
	if l.file == nil && !l.commitDone {
		return fmt.Errorf("gate ref lock handle is unavailable; ownership journal retained")
	}
	if l.file != nil && l.osLocked {
		releaseGateRefOSLock(l.file)
		l.osLocked = false
	}
	if l.file != nil {
		closeErr := l.file.Close()
		l.file = nil
		if closeErr != nil {
			l.releaseErr = fmt.Errorf("close gate ref lock: %w", closeErr)
			l.released = true
			return l.releaseErr
		}
	}
	if l.payloadPath != "" {
		if err := os.Remove(l.payloadPath); err != nil && !os.IsNotExist(err) {
			l.releaseErr = fmt.Errorf("remove gate ref payload: %w", err)
			l.released = true
			return l.releaseErr
		}
	}
	removeErr := removeGateRefLock(l.path)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		l.releaseErr = fmt.Errorf("remove gate ref lock: %w", removeErr)
		l.released = true
		return l.releaseErr
	}
	if database != nil && l.owner.RunID != "" && l.owner.OwnerGeneration != "" {
		if err := database.ClearGateRefLock(l.owner.RunID, l.owner.OwnerGeneration); err != nil {
			l.releaseErr = err
			l.released = true
			return err
		}
	}
	l.released = true
	return nil
}

func readLockedGateRef(gateDir, ref string) (string, error) {
	if strings.TrimSpace(gateDir) == "" || !isManagedGateRef(ref) {
		return "", fmt.Errorf("read locked gate ref: invalid ref")
	}
	loosePath := filepath.Join(gateDir, filepath.FromSlash(ref))
	if err := rejectSymlinkPath(gateDir, ref); err != nil {
		return "", err
	}
	looseInfo, statErr := os.Lstat(loosePath)
	if statErr == nil {
		if !looseInfo.Mode().IsRegular() {
			return "", fmt.Errorf("read locked gate ref: noncanonical loose ref %s", ref)
		}
		loose, err := os.ReadFile(loosePath)
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(loose))
		if strings.HasPrefix(value, "ref:") {
			return "", fmt.Errorf("read locked gate ref: symbolic ref %s", ref)
		}
		if value == "" || strings.ContainsAny(value, " \t\r\n") {
			return "", fmt.Errorf("read locked gate ref: malformed loose ref %s", ref)
		}
		return value, nil
	}
	if !os.IsNotExist(statErr) {
		return "", statErr
	}
	packedPath := filepath.Join(gateDir, "packed-refs")
	packedInfo, packedStatErr := os.Lstat(packedPath)
	if packedStatErr == nil && !packedInfo.Mode().IsRegular() {
		return "", fmt.Errorf("read locked gate ref: noncanonical packed refs")
	}
	packed, err := os.ReadFile(packedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("read locked gate ref: %w: %s", errGateRefAbsent, ref)
		}
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("read locked gate ref: %w: %s", errGateRefAbsent, ref)
}

func readDirectLooseGateRef(gateDir, ref string) (string, error) {
	return readDirectLooseRef(gateDir, ref)
}

func readDirectLooseWorktreeRef(workDir, ref string) (string, error) {
	gitDir, err := worktreeGitDir(workDir)
	if err != nil {
		return "", err
	}
	return readDirectLooseRef(gitDir, ref)
}

func readDirectLooseRef(gateDir, ref string) (string, error) {
	if strings.TrimSpace(gateDir) == "" || !isManagedGateRef(ref) {
		return "", fmt.Errorf("read direct gate ref: invalid ref")
	}
	if err := rejectSymlinkPath(gateDir, ref); err != nil {
		return "", err
	}
	path := filepath.Join(gateDir, filepath.FromSlash(ref))
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		packed, packedErr := packedGateRefExists(gateDir, ref)
		if packedErr != nil {
			return "", packedErr
		}
		if packed {
			return "", fmt.Errorf("read direct gate ref: packed ref %s is noncanonical", ref)
		}
		return "", fmt.Errorf("read direct gate ref: %w: %s", errGateRefAbsent, ref)
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read direct gate ref: noncanonical ref %s", ref)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	valueString := strings.TrimSpace(string(value))
	if valueString == "" || strings.HasPrefix(valueString, "ref:") || strings.ContainsAny(valueString, " \t\r\n") {
		return "", fmt.Errorf("read direct gate ref: noncanonical ref %s", ref)
	}
	return valueString, nil
}

func worktreeGitDir(workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		return "", fmt.Errorf("resolve worktree Git directory: empty path")
	}
	if osInfo, err := os.Lstat(workDir); err != nil || !osInfo.IsDir() || osInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("resolve worktree Git directory: invalid worktree")
	}
	dotGit := filepath.Join(workDir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return "", fmt.Errorf("resolve worktree Git directory: %w", err)
	}
	var gitDir string
	switch {
	case info.IsDir():
		gitDir = dotGit
	case info.Mode().IsRegular():
		data, readErr := os.ReadFile(dotGit)
		if readErr != nil {
			return "", fmt.Errorf("read worktree Git directory: %w", readErr)
		}
		value := strings.TrimSpace(string(data))
		if !strings.HasPrefix(value, "gitdir:") {
			return "", fmt.Errorf("resolve worktree Git directory: malformed .git file")
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(value, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(workDir, gitDir)
		}
	default:
		return "", fmt.Errorf("resolve worktree Git directory: noncanonical .git entry")
	}
	gitDir, err = filepath.Abs(filepath.Clean(gitDir))
	if err != nil {
		return "", fmt.Errorf("resolve worktree Git directory: %w", err)
	}
	commonDirPath := filepath.Join(gitDir, "commondir")
	if data, readErr := os.ReadFile(commonDirPath); readErr == nil {
		commonDir := strings.TrimSpace(string(data))
		if commonDir == "" {
			return "", fmt.Errorf("resolve worktree Git directory: empty commondir")
		}
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
		gitDir = commonDir
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("read worktree Git commondir: %w", readErr)
	}
	return filepath.Clean(gitDir), nil
}

func rejectSymlinkPath(gateDir, ref string) error {
	current := filepath.Clean(gateDir)
	for _, part := range strings.Split(filepath.FromSlash(ref), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect canonical gate ref path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("read locked gate ref: symlinked ref path %s", ref)
		}
	}
	return nil
}

func (l *gateRefLock) Release() error {
	if l == nil {
		return nil
	}
	return l.release(l.database)
}

func (l *gateRefLock) closeKeepJournal() {
	if l == nil || l.file == nil {
		return
	}
	if l.osLocked {
		releaseGateRefOSLock(l.file)
		l.osLocked = false
	}
	_ = l.file.Close()
	l.file = nil
}
