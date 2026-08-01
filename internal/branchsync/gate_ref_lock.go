package branchsync

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	file       *os.File
	path       string
	owner      gateRefLockOwner
	identity   string
	database   *db.DB
	osLocked   bool
	released   bool
	releaseErr error
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

func (l *gateRefLock) release(database *db.DB) error {
	if l == nil {
		return nil
	}
	if l.released {
		return l.releaseErr
	}
	if l.file == nil {
		l.released = true
		return l.releaseErr
	}
	if l.osLocked {
		releaseGateRefOSLock(l.file)
		l.osLocked = false
	}
	closeErr := l.file.Close()
	l.file = nil
	removeErr := removeGateRefLock(l.path)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		l.releaseErr = fmt.Errorf("remove gate ref lock: %w", removeErr)
		l.released = true
		return l.releaseErr
	}
	if closeErr != nil {
		l.releaseErr = fmt.Errorf("close gate ref lock: %w", closeErr)
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
