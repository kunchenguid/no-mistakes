package steps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

var rebaseSnapshotGitPaths = []string{
	"HEAD",
	"index",
	"ORIG_HEAD",
	"REBASE_HEAD",
	"AUTO_MERGE",
	"MERGE_HEAD",
	"MERGE_MSG",
	"CHERRY_PICK_HEAD",
	"REVERT_HEAD",
	"MERGE_AUTOSTASH",
	"SQUASH_MSG",
	"sequencer",
	"rebase-merge",
	"rebase-apply",
	"info/exclude",
	"logs/HEAD",
	"config.worktree",
	"commondir",
	"gitdir",
}

var rebaseProtectedGitPaths = []string{
	"ORIG_HEAD",
	"info/exclude",
	"config.worktree",
	"commondir",
	"gitdir",
}

type rebaseRefState struct {
	oid    string
	symref string
}

// rebaseRefRestoreHookContextKey is a deterministic test seam at the only
// meaningful ref race boundary: after rollback observes the attempt's value
// and immediately before Git compares and updates it.
type rebaseRefRestoreHookContextKey struct{}

func runRebaseRefRestoreHook(ctx context.Context, name string) {
	if hook, ok := ctx.Value(rebaseRefRestoreHookContextKey{}).(func(string)); ok {
		hook(name)
	}
}

// rebaseAttemptSnapshot is a transaction boundary around one write-capable
// conflict repair or completed-candidate verification. It owns every mutable
// worktree byte (including ignored paths and empty directories), the logical
// index, per-worktree operation metadata, HEAD topology, and shared refs.
type rebaseAttemptSnapshot struct {
	restoreContext context.Context
	workDir        string
	gitPaths       map[string]string
	tempDir        string
	worktreeDir    string
	gitStateDir    string
	worktreeMode   os.FileMode
	gitLinkPath    string
	gitLinkSaved   string
	gitLinkState   string

	worktreeManifest   string
	auxiliaryManifest  string
	gitStateManifest   string
	protectedGitState  string
	indexState         string
	refs               map[string]rebaseRefState
	otherWorktreeRefs  map[string]bool
	rebaseHeadName     string
	rebaseTerminalRef  string
	completedHeadRef   string
	trackedPaths       map[string]bool
	directoryModes     map[string]os.FileMode
	rebaseOnto         string
	integrityViolation error
	mu                 sync.Mutex
	cleaned            bool
}

func captureRebaseAttempt(ctx context.Context, workDir string) (*rebaseAttemptSnapshot, error) {
	gitPaths, err := resolveRebaseGitPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(workDir)
	if err != nil {
		return nil, fmt.Errorf("inspect rebase worktree: %w", err)
	}
	tempDir, err := os.MkdirTemp("", "no-mistakes-rebase-attempt-*")
	if err != nil {
		return nil, fmt.Errorf("create rebase attempt snapshot: %w", err)
	}
	snapshot := &rebaseAttemptSnapshot{
		restoreContext: context.WithoutCancel(ctx),
		workDir:        workDir,
		gitPaths:       gitPaths,
		tempDir:        tempDir,
		worktreeDir:    filepath.Join(tempDir, "worktree"),
		gitStateDir:    filepath.Join(tempDir, "git-state"),
		worktreeMode:   rootInfo.Mode(),
	}
	complete := false
	defer func() {
		if !complete {
			snapshot.cleanup()
		}
	}()
	if err := os.Mkdir(snapshot.worktreeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create rebase worktree snapshot: %w", err)
	}
	if err := snapshotRebaseWorktree(workDir, snapshot.worktreeDir); err != nil {
		return nil, err
	}
	if err := os.Mkdir(snapshot.gitStateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create rebase metadata snapshot: %w", err)
	}
	if err := snapshotRebaseGitState(gitPaths, snapshot.gitStateDir); err != nil {
		return nil, err
	}
	dotGitPath := filepath.Join(workDir, ".git")
	if dotGitInfo, dotGitErr := os.Lstat(dotGitPath); dotGitErr == nil && !dotGitInfo.IsDir() {
		snapshot.gitLinkPath = dotGitPath
		snapshot.gitLinkSaved = filepath.Join(tempDir, "worktree-git-link")
		if err := copyRebasePath(dotGitPath, snapshot.gitLinkSaved); err != nil {
			return nil, fmt.Errorf("snapshot worktree git link: %w", err)
		}
		snapshot.gitLinkState, err = rebaseTreeManifest(dotGitPath, false)
		if err != nil {
			return nil, fmt.Errorf("fingerprint worktree git link: %w", err)
		}
	} else if dotGitErr != nil {
		return nil, fmt.Errorf("inspect worktree git entry: %w", dotGitErr)
	}
	snapshot.worktreeManifest, err = rebaseTreeManifest(workDir, true)
	if err != nil {
		return nil, fmt.Errorf("fingerprint rebase worktree: %w", err)
	}
	snapshot.gitStateManifest, err = rebaseGitStateManifest(gitPaths)
	if err != nil {
		return nil, fmt.Errorf("fingerprint rebase metadata: %w", err)
	}
	snapshot.trackedPaths, err = captureRebaseTrackedPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}
	snapshot.auxiliaryManifest, err = rebaseAuxiliaryManifest(workDir, snapshot.trackedPaths)
	if err != nil {
		return nil, fmt.Errorf("fingerprint rebase auxiliary state: %w", err)
	}
	snapshot.protectedGitState, err = rebaseSelectedGitStateManifest(gitPaths, rebaseProtectedGitPaths)
	if err != nil {
		return nil, fmt.Errorf("fingerprint protected git metadata: %w", err)
	}
	snapshot.directoryModes, err = captureRebaseDirectoryModes(workDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot rebase directory modes: %w", err)
	}
	snapshot.indexState, err = captureRebaseIndexState(ctx, workDir, gitPaths["index"])
	if err != nil {
		return nil, fmt.Errorf("snapshot rebase index: %w", err)
	}
	snapshot.refs, err = captureRebaseRefs(ctx, workDir)
	if err != nil {
		return nil, err
	}
	snapshot.otherWorktreeRefs, err = captureOtherWorktreeRefs(ctx, workDir)
	if err != nil {
		return nil, err
	}
	rebaseDir, err := activeRebaseDir(ctx, workDir)
	if err != nil {
		return nil, err
	}
	if rebaseDir != "" {
		headName, readErr := os.ReadFile(filepath.Join(rebaseDir, "head-name"))
		if readErr != nil {
			return nil, fmt.Errorf("read rebase head-name: %w", readErr)
		}
		onto, readErr := os.ReadFile(filepath.Join(rebaseDir, "onto"))
		if readErr != nil {
			return nil, fmt.Errorf("read rebase onto: %w", readErr)
		}
		snapshot.rebaseHeadName = strings.TrimSpace(string(headName))
		snapshot.rebaseOnto = strings.TrimSpace(string(onto))
		if strings.HasPrefix(snapshot.rebaseHeadName, "refs/") {
			snapshot.rebaseTerminalRef, err = terminalRebaseRef(snapshot.rebaseHeadName, snapshot.refs)
			if err != nil {
				return nil, err
			}
		}
	}
	complete = true
	return snapshot, nil
}

func (snapshot *rebaseAttemptSnapshot) RestoreFailedAttempt() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.cleaned {
		return fmt.Errorf("rebase attempt snapshot was already cleaned")
	}
	if validationErr := snapshot.validateLocked(); validationErr != nil && snapshot.integrityViolation == nil {
		snapshot.integrityViolation = validationErr
	}
	if err := restoreRebaseWorktree(snapshot.workDir, snapshot.worktreeDir, snapshot.worktreeMode); err != nil {
		return err
	}
	if err := restoreRebaseGitState(snapshot.gitPaths, snapshot.gitStateDir); err != nil {
		return err
	}
	if snapshot.gitLinkPath != "" {
		if err := makeRebasePathWritable(snapshot.gitLinkPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("make mutated worktree git link removable: %w", err)
		}
		if err := os.RemoveAll(snapshot.gitLinkPath); err != nil {
			return fmt.Errorf("remove mutated worktree git link: %w", err)
		}
		if err := copyRebasePath(snapshot.gitLinkSaved, snapshot.gitLinkPath); err != nil {
			return fmt.Errorf("restore worktree git link: %w", err)
		}
	}
	if err := restoreRebaseRefs(snapshot.restoreContext, snapshot.workDir, snapshot.refs, snapshot.otherWorktreeRefs); err != nil {
		return err
	}
	return snapshot.validateLocked()
}

func (snapshot *rebaseAttemptSnapshot) ValidateSuccessfulAttempt() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.cleaned {
		return fmt.Errorf("rebase attempt snapshot was already cleaned")
	}
	var err error
	if snapshot.rebaseHeadName == "" {
		err = snapshot.validateLocked()
	} else {
		var rebaseDir string
		rebaseDir, err = activeRebaseDir(snapshot.restoreContext, snapshot.workDir)
		if err == nil && (rebaseDir != "" || len(rebaseConflictFiles(snapshot.restoreContext, snapshot.workDir)) > 0) {
			err = fmt.Errorf("successful fixer response left the rebase incomplete")
		}
		if err == nil {
			err = snapshot.validateCompletedCandidateLocked()
		}
	}
	if err != nil && snapshot.integrityViolation == nil {
		snapshot.integrityViolation = err
	}
	return err
}

func (snapshot *rebaseAttemptSnapshot) integrityError() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	return snapshot.integrityViolation
}

func (snapshot *rebaseAttemptSnapshot) validate() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.cleaned {
		return fmt.Errorf("rebase attempt snapshot was already cleaned")
	}
	return snapshot.validateLocked()
}

func (snapshot *rebaseAttemptSnapshot) validateLocked() error {
	if snapshot.gitLinkPath != "" {
		gitLinkState, err := rebaseTreeManifest(snapshot.gitLinkPath, false)
		if err != nil {
			return fmt.Errorf("validate worktree git link: %w", err)
		}
		if gitLinkState != snapshot.gitLinkState {
			return fmt.Errorf("worktree git link differs from its sealed snapshot")
		}
	}
	worktreeManifest, err := rebaseTreeManifest(snapshot.workDir, true)
	if err != nil {
		return err
	}
	if worktreeManifest != snapshot.worktreeManifest {
		return fmt.Errorf("rebase candidate worktree differs from its sealed snapshot")
	}
	gitStateManifest, err := rebaseGitStateManifest(snapshot.gitPaths)
	if err != nil {
		return err
	}
	if gitStateManifest != snapshot.gitStateManifest {
		return fmt.Errorf("rebase candidate metadata or HEAD topology differs from its sealed snapshot")
	}
	indexState, err := captureRebaseIndexState(snapshot.restoreContext, snapshot.workDir, snapshot.gitPaths["index"])
	if err != nil {
		return fmt.Errorf("validate rebase candidate index: %w", err)
	}
	if indexState != snapshot.indexState {
		return fmt.Errorf("rebase candidate index differs from its sealed snapshot")
	}
	refs, err := captureRebaseRefs(snapshot.restoreContext, snapshot.workDir)
	if err != nil {
		return err
	}
	if !equalRebaseRefs(refs, snapshot.refs) {
		return fmt.Errorf("rebase candidate shared refs differ from their sealed snapshot")
	}
	return nil
}

func (snapshot *rebaseAttemptSnapshot) validateCompletedCandidateLocked() error {
	if snapshot.gitLinkPath != "" {
		gitLinkState, err := rebaseTreeManifest(snapshot.gitLinkPath, false)
		if err != nil {
			return fmt.Errorf("validate worktree git link: %w", err)
		}
		if gitLinkState != snapshot.gitLinkState {
			return fmt.Errorf("worktree git link differs from its sealed snapshot")
		}
	}
	status, err := git.Run(snapshot.restoreContext, snapshot.workDir, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("validate completed rebase tracked state: %w", err)
	}
	if status != "" {
		return fmt.Errorf("completed rebase left tracked worktree or index changes")
	}
	currentTrackedPaths, err := captureRebaseTrackedPaths(snapshot.restoreContext, snapshot.workDir)
	if err != nil {
		return fmt.Errorf("validate completed rebase tracked paths: %w", err)
	}
	auxiliaryManifest, err := rebaseAuxiliaryManifest(snapshot.workDir, currentTrackedPaths)
	if err != nil {
		return fmt.Errorf("validate completed rebase auxiliary state: %w", err)
	}
	if auxiliaryManifest != snapshot.auxiliaryManifest {
		return fmt.Errorf("completed rebase mutated ignored, untracked, empty-directory, or directory-mode state")
	}
	protectedGitState, err := rebaseSelectedGitStateManifest(snapshot.gitPaths, rebaseProtectedGitPaths)
	if err != nil {
		return fmt.Errorf("validate protected git metadata: %w", err)
	}
	currentDirectoryModes, err := captureRebaseDirectoryModes(snapshot.workDir)
	if err != nil {
		return fmt.Errorf("validate completed rebase directory modes: %w", err)
	}
	for path, beforeMode := range snapshot.directoryModes {
		if afterMode, ok := currentDirectoryModes[path]; ok && afterMode != beforeMode {
			return fmt.Errorf("completed rebase mutated directory mode for %s", path)
		}
	}
	if protectedGitState != snapshot.protectedGitState {
		return fmt.Errorf("completed rebase mutated protected git metadata")
	}
	return snapshot.validateCompletedTopologyLocked()
}

// validateCompletedTopology permits only the ref transition that git rebase
// itself owns (head-name). Every other shared ref and the resulting symbolic or
// detached HEAD topology remain protected from the fixer.
func (snapshot *rebaseAttemptSnapshot) validateCompletedTopology() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.cleaned {
		return fmt.Errorf("rebase attempt snapshot was already cleaned")
	}
	return snapshot.validateCompletedTopologyLocked()
}

func (snapshot *rebaseAttemptSnapshot) validateCompletedTopologyLocked() error {
	refs, err := captureRebaseRefs(snapshot.restoreContext, snapshot.workDir)
	if err != nil {
		return err
	}
	allowedRef := snapshot.rebaseTerminalRef
	for name, before := range snapshot.refs {
		if name == allowedRef {
			continue
		}
		after, ok := refs[name]
		if !ok || !sameProtectedRebaseRef(before, after) {
			return fmt.Errorf("completed rebase mutated protected ref %s", name)
		}
	}
	for name := range refs {
		if name != allowedRef {
			if _, ok := snapshot.refs[name]; !ok {
				return fmt.Errorf("completed rebase created protected ref %s", name)
			}
		}
	}
	if allowedRef != "" {
		allowed, ok := refs[allowedRef]
		if !ok || allowed.symref != "" {
			return fmt.Errorf("completed rebase did not preserve terminal branch ref %s", allowedRef)
		}
	}
	headRef, err := captureRebaseHeadRef(snapshot.restoreContext, snapshot.workDir)
	if err != nil {
		return err
	}
	if snapshot.completedHeadRef != "" {
		terminalRef, err := terminalRebaseRef(snapshot.completedHeadRef, snapshot.refs)
		if err != nil {
			return err
		}
		if terminalRef != allowedRef {
			return fmt.Errorf("completed rebase terminal HEAD ref %q, want %q", terminalRef, allowedRef)
		}
		if headRef != snapshot.completedHeadRef {
			if headRef != allowedRef && headRef != snapshot.rebaseHeadName {
				return fmt.Errorf("completed rebase HEAD topology %q, want %q", headRef, snapshot.completedHeadRef)
			}
			if _, err := git.Run(snapshot.restoreContext, snapshot.workDir, "symbolic-ref", "HEAD", snapshot.completedHeadRef); err != nil {
				return fmt.Errorf("restore completed rebase symbolic HEAD topology: %w", err)
			}
			headRef = snapshot.completedHeadRef
		}
	} else if allowedRef != "" {
		return fmt.Errorf("completed detached rebase unexpectedly advanced branch ref %s", allowedRef)
	}
	if headRef != snapshot.completedHeadRef {
		return fmt.Errorf("completed rebase HEAD topology %q, want %q", headRef, snapshot.completedHeadRef)
	}
	if snapshot.rebaseOnto == "" {
		return fmt.Errorf("completed rebase lost its sealed onto commit")
	}
	headOID, err := git.HeadSHA(snapshot.restoreContext, snapshot.workDir)
	if err != nil {
		return fmt.Errorf("resolve completed rebase HEAD: %w", err)
	}
	if headOID == snapshot.rebaseOnto {
		return fmt.Errorf("completed rebase dropped every replayed commit onto %s", snapshot.rebaseOnto)
	}
	treeDiff, err := git.Run(snapshot.restoreContext, snapshot.workDir, "diff", "--name-only", snapshot.rebaseOnto, "HEAD")
	if err != nil {
		return fmt.Errorf("compare completed rebase tree with sealed onto commit: %w", err)
	}
	if treeDiff == "" {
		return fmt.Errorf("completed rebase tree is identical to sealed onto commit %s", snapshot.rebaseOnto)
	}
	if _, err := git.Run(snapshot.restoreContext, snapshot.workDir, "merge-base", "--is-ancestor", snapshot.rebaseOnto, "HEAD"); err != nil {
		return fmt.Errorf("completed rebase HEAD does not descend from sealed onto commit %s: %w", snapshot.rebaseOnto, err)
	}
	return nil
}

func (snapshot *rebaseAttemptSnapshot) cleanup() {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.cleaned {
		return
	}
	snapshot.cleaned = true
	_ = makeRebasePathWritable(snapshot.tempDir)
	_ = os.RemoveAll(snapshot.tempDir)
}

func activeRebaseDir(ctx context.Context, workDir string) (string, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		path, err := git.Run(ctx, workDir, "rev-parse", "--git-path", name)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", name, err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		info, statErr := os.Stat(path)
		if statErr == nil && info.IsDir() {
			return path, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect %s: %w", name, statErr)
		}
	}
	return "", nil
}

func snapshotRebaseWorktree(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read rebase worktree: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := copyRebasePath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return fmt.Errorf("snapshot rebase worktree path %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func restoreRebaseWorktree(destination, source string, mode os.FileMode) error {
	if err := os.Chmod(destination, mode.Perm()|0o700); err != nil {
		return fmt.Errorf("make rebase worktree root writable: %w", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read mutated rebase worktree: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := makeRebasePathWritable(filepath.Join(destination, entry.Name())); err != nil {
			return fmt.Errorf("make mutated rebase path %q removable: %w", entry.Name(), err)
		}
		if err := os.RemoveAll(filepath.Join(destination, entry.Name())); err != nil {
			return fmt.Errorf("remove mutated rebase path %q: %w", entry.Name(), err)
		}
	}
	if err := snapshotRebaseWorktree(source, destination); err != nil {
		return fmt.Errorf("restore rebase worktree: %w", err)
	}
	return os.Chmod(destination, mode)
}

func resolveRebaseGitPaths(ctx context.Context, workDir string) (map[string]string, error) {
	paths := make(map[string]string, len(rebaseSnapshotGitPaths))
	for _, name := range rebaseSnapshotGitPaths {
		path, err := git.Run(ctx, workDir, "rev-parse", "--git-path", name)
		if err != nil {
			return nil, fmt.Errorf("resolve rebase metadata path %q: %w", name, err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		paths[name] = filepath.Clean(path)
	}
	return paths, nil
}

func snapshotRebaseGitState(gitPaths map[string]string, destination string) error {
	for _, name := range rebaseSnapshotGitPaths {
		source := gitPaths[name]
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect rebase metadata %q: %w", name, err)
		}
		if err := copyRebasePath(source, filepath.Join(destination, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("snapshot rebase metadata %q: %w", name, err)
		}
	}
	return nil
}

func restoreRebaseGitState(gitPaths map[string]string, source string) error {
	for _, name := range rebaseSnapshotGitPaths {
		destination := gitPaths[name]
		if err := makeRebasePathWritable(destination); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("make mutated rebase metadata %q removable: %w", name, err)
		}
		if err := os.RemoveAll(destination); err != nil {
			return fmt.Errorf("remove mutated rebase metadata %q: %w", name, err)
		}
		saved := filepath.Join(source, filepath.FromSlash(name))
		if _, err := os.Lstat(saved); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect saved rebase metadata %q: %w", name, err)
		}
		if err := copyRebasePath(saved, destination); err != nil {
			return fmt.Errorf("restore rebase metadata %q: %w", name, err)
		}
	}
	return nil
}

func copyRebasePath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if info.IsDir() {
		if err := os.Mkdir(destination, info.Mode().Perm()|0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := os.Chmod(destination, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyRebasePath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(destination, info.Mode())
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported filesystem object %s", info.Mode())
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode())
}

func makeRebasePathWritable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		if err := os.Chmod(path, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := makeRebasePathWritable(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if info.Mode().IsRegular() {
		return os.Chmod(path, info.Mode().Perm()|0o600)
	}
	return nil
}

func rebaseTreeManifest(root string, skipDotGit bool) (string, error) {
	hash := sha256.New()
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(hash, ".\x00%#o\x00", rootInfo.Mode())
	if err := writeRebaseManifestPayload(hash, root, ".", rootInfo); err != nil {
		return "", err
	}
	hash.Write([]byte{0})
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if skipDotGit && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, rel := range paths {
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%#o\x00", filepath.ToSlash(rel), info.Mode())
		if err := writeRebaseManifestPayload(hash, path, rel, info); err != nil {
			return "", err
		}
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func captureRebaseTrackedPaths(ctx context.Context, workDir string) (map[string]bool, error) {
	output, err := git.Run(ctx, workDir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list tracked rebase candidate paths: %w", err)
	}
	paths := make(map[string]bool)
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths[filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))] = true
		}
	}
	return paths, nil
}

func captureRebaseDirectoryModes(root string) (map[string]os.FileMode, error) {
	modes := make(map[string]os.FileMode)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			modes[filepath.ToSlash(rel)] = info.Mode()
		}
		return nil
	})
	return modes, err
}

func rebaseAuxiliaryManifest(root string, trackedPaths map[string]bool) (string, error) {
	hash := sha256.New()
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(hash, ".\x00%#o\x00", rootInfo.Mode())

	trackedDirectories := make(map[string]bool)
	for path := range trackedPaths {
		for dir := filepath.Dir(filepath.FromSlash(path)); dir != "."; dir = filepath.Dir(dir) {
			trackedDirectories[filepath.ToSlash(dir)] = true
		}
	}
	auxiliaryDirectories := make(map[string]bool)
	var directories []string
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		normalizedRel := filepath.ToSlash(rel)
		if info.IsDir() {
			directories = append(directories, rel)
			return nil
		}
		if trackedPaths[normalizedRel] {
			return nil
		}
		paths = append(paths, rel)
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			auxiliaryDirectories[filepath.ToSlash(dir)] = true
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	for _, rel := range directories {
		normalizedRel := filepath.ToSlash(rel)
		if !trackedDirectories[normalizedRel] || auxiliaryDirectories[normalizedRel] {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	for _, rel := range paths {
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%#o\x00", filepath.ToSlash(rel), info.Mode())
		if err := writeRebaseManifestPayload(hash, path, rel, info); err != nil {
			return "", err
		}
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func writeRebaseManifestPayload(output io.Writer, path, displayPath string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = io.WriteString(output, target)
		return err
	case info.IsDir():
		return nil
	case info.Mode().IsRegular():
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	default:
		return fmt.Errorf("unsupported filesystem object %s at %s", info.Mode(), displayPath)
	}
}

func captureRebaseIndexState(ctx context.Context, workDir, indexPath string) (string, error) {
	tree, treeErr := git.Run(ctx, workDir, "write-tree")
	if treeErr == nil {
		return "tree:" + tree, nil
	}
	unmerged, err := git.Run(ctx, workDir, "ls-files", "--unmerged", "-z")
	if err != nil {
		return "", errors.Join(treeErr, err)
	}
	if unmerged == "" {
		return "", treeErr
	}
	manifest, err := rebaseTreeManifest(indexPath, false)
	if err != nil {
		return "", err
	}
	return "unmerged:" + manifest, nil
}

func rebaseGitStateManifest(gitPaths map[string]string) (string, error) {
	return rebaseSelectedGitStateManifest(gitPaths, rebaseSnapshotGitPaths)
}

func rebaseSelectedGitStateManifest(gitPaths map[string]string, names []string) (string, error) {
	hash := sha256.New()
	for _, name := range names {
		if name == "index" {
			continue // index stat-cache bytes are not candidate semantics.
		}
		path := gitPaths[name]
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			fmt.Fprintf(hash, "%s\x00missing\x00", name)
			continue
		} else if err != nil {
			return "", err
		}
		manifest, err := rebaseTreeManifest(path, false)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00", name, manifest)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func captureRebaseHeadRef(ctx context.Context, workDir string) (string, error) {
	path, err := git.Run(ctx, workDir, "rev-parse", "--git-path", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve rebase HEAD path: %w", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read rebase HEAD topology: %w", err)
	}
	const symbolicPrefix = "ref: "
	value := strings.TrimSpace(string(content))
	if strings.HasPrefix(value, symbolicPrefix) {
		return strings.TrimPrefix(value, symbolicPrefix), nil
	}
	return "", nil
}

func terminalRebaseRef(name string, refs map[string]rebaseRefState) (string, error) {
	visited := make(map[string]bool)
	for {
		if visited[name] {
			return "", fmt.Errorf("rebase head-name contains a symbolic-ref cycle at %s", name)
		}
		visited[name] = true
		state, ok := refs[name]
		if !ok {
			return "", fmt.Errorf("rebase head-name ref %s is absent from the sealed refs", name)
		}
		if state.symref == "" {
			return name, nil
		}
		name = state.symref
	}
}

func captureOtherWorktreeRefs(ctx context.Context, workDir string) (map[string]bool, error) {
	output, err := git.Run(ctx, workDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list linked worktrees for rebase snapshot: %w", err)
	}
	absoluteWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve rebase worktree path: %w", err)
	}
	refs := make(map[string]bool)
	currentWorktree := ""
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			currentWorktree = filepath.Clean(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch ") && currentWorktree != filepath.Clean(absoluteWorkDir):
			refs[strings.TrimPrefix(line, "branch ")] = true
		}
	}
	return refs, nil
}

func captureRebaseRefs(ctx context.Context, workDir string) (map[string]rebaseRefState, error) {
	output, err := git.Run(ctx, workDir, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)")
	if err != nil {
		return nil, fmt.Errorf("snapshot shared refs: %w", err)
	}
	refs := make(map[string]rebaseRefState)
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("parse shared ref snapshot line %q", line)
		}
		state := rebaseRefState{oid: parts[1]}
		if len(parts) == 3 {
			state.symref = parts[2]
		}
		refs[parts[0]] = state
	}
	return refs, nil
}

func restoreRebaseRefs(
	ctx context.Context,
	workDir string,
	want map[string]rebaseRefState,
	otherWorktreeRefs map[string]bool,
) error {
	got, err := captureRebaseRefs(ctx, workDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(got)+len(want))
	seen := make(map[string]bool)
	for name := range got {
		names = append(names, name)
		seen[name] = true
	}
	for name := range want {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		before, existed := want[name]
		after, exists := got[name]
		if existed && exists && before == after {
			continue
		}
		if otherWorktreeRefs[name] {
			return fmt.Errorf("concurrent shared-ref change prevented exact rollback of %s", name)
		}
	}
	for _, name := range names {
		before, existed := want[name]
		after, exists := got[name]
		if existed && exists && before == after {
			continue
		}
		if err := restoreRebaseRefCAS(ctx, workDir, name, before, existed, after, exists); err != nil {
			return err
		}
	}
	finalRefs, err := captureRebaseRefs(ctx, workDir)
	if err != nil {
		return err
	}
	if !equalRebaseRefs(finalRefs, want) {
		return fmt.Errorf("concurrent shared-ref change prevented exact rollback after restoration")
	}
	return nil
}

// restoreRebaseRefCAS restores exactly the value observed after the agent, and
// only if the live ref still holds that observed value. It is a compare-and-swap
// through Git-compatible ref transactions, so it works identically for the
// loose, packed, and reftable backends:
//
//   - Direct refs: update-ref's expected-old-oid form (a single atomic ref
//     transaction). A concurrent object-id movement after observation is
//     rejected, preserving the external update.
//   - Symbolic refs: Git's symref-update / symref-delete transaction verbs, so
//     the expected target and the replacement commit as one backend transaction
//     with no observe-then-unconditional overwrite. Git older than 2.46 lacks
//     these verbs, so a symbolic restoration fails closed instead of clobbering.
//   - Restoring a direct baseline while the ref is observed symbolic fails
//     closed: Git cannot atomically turn a symbolic ref back into a direct ref
//     (symref-* never yields a direct ref, and delete+create of one ref in a
//     single transaction is rejected), so an unconditional overwrite would race.
//
// Known backend limitation: when the observed value is a direct object id and a
// concurrent writer replaces the ref with a symbolic ref that resolves to the
// same object id, Git's no-deref old-oid verification resolves the symref before
// comparing and accepts the write, losing that symbolic topology. This holds on
// every current Git backend (files, packed, and reftable, verified through Git
// 2.46), because no public plumbing verb performs an "update only if this exact
// ref is direct at this oid" check. Fully closing it needs attempt-private ref
// isolation (a dedicated mutation namespace so the agent can never touch the
// shared ref in the first place), which is future work. Direct recovery is not
// weakened for the common case: failing every direct rollback to defend this
// narrow interleaving would strand ordinary failed-attempt corruption, which is
// strictly worse.
func restoreRebaseRefCAS(
	ctx context.Context,
	workDir, name string,
	before rebaseRefState,
	existed bool,
	after rebaseRefState,
	exists bool,
) error {
	currentRefs, err := captureRebaseRefs(ctx, workDir)
	if err != nil {
		return err
	}
	current, currentExists := currentRefs[name]
	if currentExists != exists || (exists && current != after) {
		return fmt.Errorf("shared ref %s changed concurrently while restoring rebase candidate", name)
	}

	runRebaseRefRestoreHook(ctx, name)

	if !existed {
		if !exists {
			return nil
		}
		if after.symref != "" {
			transaction := fmt.Sprintf(
				"start\nsymref-delete %s %s\nprepare\ncommit\n",
				name,
				after.symref,
			)
			if err := runRebaseRefTransaction(ctx, workDir, transaction); err != nil {
				return fmt.Errorf("atomically remove attempt-created symbolic shared ref %s: %w", name, err)
			}
			return nil
		}
		if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", "-d", name, after.oid); err != nil {
			return fmt.Errorf("shared ref %s changed concurrently while removing an attempt-created ref: %w", name, err)
		}
		return nil
	}
	if before.symref != "" {
		oldCondition := "oid " + strings.Repeat("0", len(before.oid))
		if exists {
			if after.symref != "" {
				oldCondition = "ref " + after.symref
			} else {
				oldCondition = "oid " + after.oid
			}
		}
		transaction := fmt.Sprintf(
			"start\nsymref-update %s %s %s\nprepare\ncommit\n",
			name,
			before.symref,
			oldCondition,
		)
		if err := runRebaseRefTransaction(ctx, workDir, transaction); err != nil {
			return fmt.Errorf("atomically restore symbolic shared ref %s: %w", name, err)
		}
		return nil
	}

	if after.symref != "" {
		return fmt.Errorf(
			"cannot atomically restore direct shared ref %s from symbolic target %s; refusing a non-transactional overwrite",
			name,
			after.symref,
		)
	}

	zeroOID := strings.Repeat("0", len(before.oid))
	if exists {
		if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", name, before.oid, after.oid); err != nil {
			return fmt.Errorf("shared ref %s changed concurrently while restoring its sealed value: %w", name, err)
		}
	} else {
		if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", name, before.oid, zeroOID); err != nil {
			return fmt.Errorf("shared ref %s changed concurrently while restoring a deleted ref: %w", name, err)
		}
	}
	return nil
}

func runRebaseRefTransaction(ctx context.Context, workDir, transaction string) error {
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--no-deref", "--stdin")
	cmd.Dir = workDir
	cmd.Env = append(git.NonInteractiveEnv(workDir), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(transaction)
	winproc.Harden(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if strings.Contains(message, "unknown command: symref-") {
			return errors.New("installed Git does not support atomic symbolic-ref transactions (requires Git 2.46 or newer)")
		}
		return fmt.Errorf("git update-ref transaction: %w: %s", err, message)
	}
	return nil
}

func sameProtectedRebaseRef(before, after rebaseRefState) bool {
	if before.symref != "" || after.symref != "" {
		return before.symref == after.symref
	}
	return before.oid == after.oid
}

func equalRebaseRefs(left, right map[string]rebaseRefState) bool {
	if len(left) != len(right) {
		return false
	}
	for name, state := range left {
		if right[name] != state {
			return false
		}
	}
	return true
}
