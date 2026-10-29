package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// DirtyWorkRefPrefix is the gate-local ref namespace for a checkout's
// uncommitted work. It is not a custody recovery anchor: those live under
// refs/no-mistakes/recover/ and record commits, which cannot represent an
// index, unstaged edits, or untracked files.
const DirtyWorkRefPrefix = "refs/no-mistakes/dirty/"

// ErrDirtyWorkRejected means capture or verification was refused, including
// when a clean checkout's restoration cannot be verified. Callers keep it.
var ErrDirtyWorkRejected = errors.New("rejected dirty work capture")

// DirtyWorkRequest names the checkout to snapshot and the repository that
// keeps the snapshot after that checkout is removed.
type DirtyWorkRequest struct {
	// GateDir is the repository that must own Worktree and that stores the
	// snapshot ref. For a run checkout this is the bare gate.
	GateDir string
	// Worktree is the checkout about to be removed.
	Worktree string
	// RunID becomes the final component of the snapshot ref.
	RunID string
	// BaseSHA is the run's recorded base when the caller knows it. It is
	// stored in the snapshot message and is not required to exist as an object.
	BaseSHA string
}

// DirtyWorkSnapshot is a verified stash-shaped commit stored outside the
// checkout. Its first parent is the checkout HEAD, its second parent records
// the index, and a third parent records owned untracked files when any exist.
type DirtyWorkSnapshot struct {
	Ref    string
	Commit string
	Head   string
	Base   string
}

// DirtyWorkRef is the gate ref for one run's uncommitted snapshot.
func DirtyWorkRef(runID string) string {
	return DirtyWorkRefPrefix + runID
}

// CaptureDirtyWork snapshots staged changes, unstaged changes, and untracked
// files that are not ignored. A clean checkout returns a nil snapshot with no
// error only after its verification checks pass. The snapshot ref is created
// only after the trees match the checkout; any error, including cancellation,
// means the checkout must stay.
// Secret-bearing paths and paths that resolve outside the checkout are
// refused before any of their bytes are written into the object store.
// Symlinks are stored as their link text. Sockets, fifos, and devices are
// refused without being opened.
func CaptureDirtyWork(ctx context.Context, req DirtyWorkRequest) (*DirtyWorkSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wt := req.Worktree
	info, err := os.Stat(wt)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat checkout: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: checkout is not a directory", ErrDirtyWorkRejected)
	}
	// A leftover directory that is not a checkout has no index or untracked
	// work to snapshot. Callers still remove it, including the orphan sweep's
	// directory fallback when git worktree remove cannot.
	if _, err := os.Lstat(filepath.Join(wt, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat checkout git dir: %w", err)
	}
	if err := assertCheckoutOwned(ctx, req.GateDir, wt); err != nil {
		return nil, err
	}

	entries, err := checkoutDirtyEntries(ctx, wt)
	if err != nil {
		return nil, err
	}
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, assertCheckoutClean(ctx, wt, nil)
	}
	ref := DirtyWorkRef(req.RunID)
	if _, err := Run(ctx, req.GateDir, "check-ref-format", ref); err != nil {
		return nil, fmt.Errorf("snapshot ref %s: %w", ref, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snap, err := writeDirtySnapshot(ctx, req, ref, entries)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// DirtyWorkStillMatches compares the checkout with snap at verification time.
// A nil snapshot matches only a clean checkout, a missing path, or a directory
// that is not a checkout. Any error requires retention; success alone cannot
// prove stability through deletion in the presence of independent writers.
func DirtyWorkStillMatches(ctx context.Context, worktree string, snap *DirtyWorkSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(worktree)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: checkout is not a directory", ErrDirtyWorkRejected)
	}
	if _, err := os.Lstat(filepath.Join(worktree, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat checkout git dir: %w", err)
	}
	if snap == nil {
		return assertCheckoutClean(ctx, worktree, nil)
	}
	head, indexTree, workTree, untrackedTree, err := readSnapshotTrees(ctx, worktree, snap.Commit)
	if err != nil {
		return err
	}
	if head != snap.Head {
		return fmt.Errorf("dirty work snapshot head does not match the recorded checkout")
	}
	config, err := readDirtyWorkRestoreConfig(ctx, worktree, snap.Commit)
	if err != nil {
		return err
	}
	scratch, err := newDirtyWorkScratch(ctx)
	if err != nil {
		return err
	}
	defer scratch.close(ctx)
	tmp := scratch.path
	return recheckDirtyWork(ctx, worktree, tmp, head, indexTree, workTree, untrackedTree, config, nil)
}

// RestoreDirtyWork reapplies a snapshot onto a checkout already at the
// snapshot's recorded HEAD. The index and the working tree are both restored.
func RestoreDirtyWork(ctx context.Context, worktree, commit string) error {
	head, err := Run(ctx, worktree, "rev-parse", "--verify", commit+"^1")
	if err != nil {
		return fmt.Errorf("read snapshot head: %w", err)
	}
	current, err := Run(ctx, worktree, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("read checkout head: %w", err)
	}
	if current != head {
		return fmt.Errorf("checkout HEAD %s is not the snapshot head %s", current, head)
	}
	config, err := readDirtyWorkRestoreConfig(ctx, worktree, commit)
	if err != nil {
		return err
	}
	if _, err := Run(ctx, worktree, withSymlinks(config.args("stash", "apply", "--index", commit)...)...); err != nil {
		return fmt.Errorf("apply dirty work snapshot: %w", err)
	}
	return nil
}

type dirtyEntry struct {
	code string
	rel  string
	kind string
}

func parsePorcelainZ(out []byte) ([]dirtyEntry, error) {
	if len(out) == 0 {
		return nil, nil
	}
	var entries []dirtyEntry
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if len(raw) < 4 || raw[2] != ' ' {
			return nil, fmt.Errorf("%w: unreadable status entry", ErrDirtyWorkRejected)
		}
		code := string(raw[:2])
		if code == "R " || code == "C " || strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {
			return nil, fmt.Errorf("%w: rename or copy in status; refusing a partial snapshot", ErrDirtyWorkRejected)
		}
		entries = append(entries, dirtyEntry{code: code, rel: string(raw[3:])})
	}
	return entries, nil
}

func porcelainEntries(ctx context.Context, wt string) ([]dirtyEntry, error) {
	status, err := RunRaw(ctx, wt, gitStatusArgs()...)
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(status)
}

// checkoutDirtyEntries is the status list plus tracked edits that status hides.
// assume-unchanged and skip-worktree keep a real worktree change out of
// porcelain, and both the clean short-circuit and the pre-removal check
// read this list.
func checkoutDirtyEntries(ctx context.Context, wt string) ([]dirtyEntry, error) {
	if err := rejectIndexIntent(ctx, wt, nil); err != nil {
		return nil, err
	}
	entries, err := porcelainEntries(ctx, wt)
	if err != nil {
		return nil, err
	}
	return appendTrackedByteEdits(ctx, wt, entries)
}

func gitStatusArgs() []string {
	return withSymlinks("status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
}

// withSymlinks forces symlink comparison. Windows git defaults
// core.symlinks off, which reads a real symlink as a mismatch or as a
// plain file and then refuses the snapshot or restores the link text as
// a regular file.
func withSymlinks(args ...string) []string {
	out := make([]string, 0, len(args)+2)
	out = append(out, "-c", "core.symlinks=true")
	out = append(out, args...)
	return out
}

func appendTrackedByteEdits(ctx context.Context, wt string, entries []dirtyEntry) ([]dirtyEntry, error) {
	out, err := RunRaw(ctx, wt, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	fileMode, err := checkoutFileMode(ctx, wt)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]int, len(entries))
	for i := range entries {
		seen[filepath.ToSlash(entries[i].rel)] = i
	}
	err = walkIndexBlobs(out, func(mode, rel, sha string) error {
		differs, exists, err := trackedPathDiffers(ctx, wt, rel, mode, sha, fileMode, nil)
		if err != nil {
			return err
		}
		if !differs {
			if i, ok := seen[filepath.ToSlash(rel)]; ok && entries[i].code != "??" {
				entries[i].code = entries[i].code[:1] + " "
			}
			return nil
		}
		if mode == "160000" {
			return fmt.Errorf("%w: dirty submodule %s cannot be snapshotted", ErrDirtyWorkRejected, rel)
		}
		slash := filepath.ToSlash(rel)
		if i, ok := seen[slash]; ok {
			entries[i].code = overlayHiddenWorktree(entries[i].code, exists)
			return nil
		}
		code := " M"
		if !exists {
			code = " D"
		}
		entries = append(entries, dirtyEntry{code: code, rel: rel})
		seen[slash] = len(entries) - 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry.code != "  " {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

func overlayHiddenWorktree(code string, exists bool) string {
	if len(code) != 2 || code[1] != ' ' {
		return code
	}
	col := byte('M')
	if !exists {
		col = 'D'
	}
	return string([]byte{code[0], col})
}

func trackedPathDiffers(ctx context.Context, wt, rel, mode, sha string, fileMode bool, observed map[string]os.FileInfo) (differs, exists bool, err error) {
	full := filepath.Join(wt, filepath.FromSlash(rel))
	if err := assertPathOwned(canonicalPath(wt), full); err != nil {
		return false, false, fmt.Errorf("%w: %s", ErrDirtyWorkRejected, err)
	}
	info, lerr := os.Lstat(full)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			if mode == "160000" {
				return false, false, nil
			}
			return true, false, nil
		}
		return false, false, lerr
	}
	if mode == "160000" {
		if !info.IsDir() {
			return true, true, nil
		}
		if _, err := os.Lstat(filepath.Join(full, ".git")); errors.Is(err, os.ErrNotExist) {
			entries, err := os.ReadDir(full)
			return len(entries) != 0, true, err
		} else if err != nil {
			return false, true, err
		}
		head, err := Run(ctx, full, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return false, true, err
		}
		if head != sha {
			return true, true, nil
		}
		if err := assertCheckoutClean(ctx, full, observed); err != nil {
			return false, true, fmt.Errorf("%w: dirty submodule %s: %w", ErrDirtyWorkRejected, rel, err)
		}
		if observed != nil {
			index, err := absoluteGitPath(ctx, full, "index")
			if err != nil {
				return false, true, err
			}
			info, err := os.Lstat(index)
			if err != nil {
				return false, true, err
			}
			observed[index] = info
		}
		return false, true, nil
	}
	var actual []byte
	if info.Mode()&os.ModeSymlink != 0 {
		if mode != "120000" {
			return true, true, nil
		}
		target, err := os.Readlink(full)
		if err != nil {
			return false, true, err
		}
		sum := sha256.Sum256([]byte(filepath.ToSlash(target)))
		actual = sum[:]
	} else {
		if !info.Mode().IsRegular() || (mode != "100644" && mode != "100755") {
			return true, true, nil
		}
		if fileMode && (info.Mode()&0o111 != 0) != (mode == "100755") {
			return true, true, nil
		}
		actual, err = hashRegularBytes(ctx, full)
		if err != nil {
			return false, true, err
		}
	}
	expected := sha256.New()
	args := []string{"cat-file", "--filters", "--path=" + filepath.ToSlash(rel), sha}
	if mode == "120000" {
		args = []string{"cat-file", "blob", sha}
	}
	if err := gitStreamTo(ctx, wt, nil, expected, args...); err != nil {
		return false, true, err
	}
	return !bytes.Equal(actual, expected.Sum(nil)), true, nil
}

func hashRegularBytes(ctx context.Context, full string) ([]byte, error) {
	file, err := openRegular(full)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	digest := sha256.New()
	buf := make([]byte, secretReadChunk)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buf)
		_, _ = digest.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return digest.Sum(nil), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

type dirtyWorkConfig struct {
	autoCRLF string
	eol      string
}

func (c dirtyWorkConfig) args(args ...string) []string {
	return append([]string{"-c", "core.autocrlf=" + c.autoCRLF, "-c", "core.eol=" + c.eol, "-c", "apply.whitespace=nowarn"}, args...)
}

func checkoutFileMode(ctx context.Context, wt string) (bool, error) {
	value, err := Run(ctx, wt, "config", "--type=bool", "--default", "true", "--get", "core.filemode")
	return value == "true", err
}

func readDirtyWorkConfig(ctx context.Context, wt string) (dirtyWorkConfig, error) {
	autoCRLF, err := Run(ctx, wt, "config", "--default", "false", "--get", "core.autocrlf")
	if err != nil {
		return dirtyWorkConfig{}, err
	}
	if strings.EqualFold(autoCRLF, "input") {
		autoCRLF = "input"
	} else {
		autoCRLF, err = Run(ctx, wt, "config", "--type=bool", "--default", "false", "--get", "core.autocrlf")
		if err != nil {
			return dirtyWorkConfig{}, err
		}
	}
	eol, err := Run(ctx, wt, "config", "--default", "native", "--get", "core.eol")
	if err != nil {
		return dirtyWorkConfig{}, err
	}
	eol = strings.ToLower(eol)
	if eol == "native" {
		eol = "lf"
		if runtime.GOOS == "windows" {
			eol = "crlf"
		}
	}
	config := dirtyWorkConfig{autoCRLF: autoCRLF, eol: eol}
	return config, config.validate()
}

func (c dirtyWorkConfig) validate() error {
	if (c.autoCRLF != "true" && c.autoCRLF != "false" && c.autoCRLF != "input") || (c.eol != "lf" && c.eol != "crlf") {
		return fmt.Errorf("%w: snapshot has no supported restore configuration", ErrDirtyWorkRejected)
	}
	return nil
}

func readDirtyWorkRestoreConfig(ctx context.Context, wt, commit string) (dirtyWorkConfig, error) {
	body, err := Run(ctx, wt, "log", "-1", "--format=%B", commit)
	if err != nil {
		return dirtyWorkConfig{}, err
	}
	var config dirtyWorkConfig
	var seenAutoCRLF, seenEOL bool
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(line, "restore-core-autocrlf: "); ok {
			if seenAutoCRLF {
				return config, fmt.Errorf("%w: duplicate restore configuration", ErrDirtyWorkRejected)
			}
			seenAutoCRLF = true
			config.autoCRLF = value
		}
		if value, ok := strings.CutPrefix(line, "restore-core-eol: "); ok {
			if seenEOL {
				return config, fmt.Errorf("%w: duplicate restore configuration", ErrDirtyWorkRejected)
			}
			seenEOL = true
			config.eol = value
		}
	}
	return config, config.validate()
}

// dirtyWorkLocks stripes checkouts so two cleanups of one path cannot
// interleave, without retaining a lock per path for the life of the daemon.
const dirtyWorkLockStripes = 64

var dirtyWorkLocks = func() [dirtyWorkLockStripes]chan struct{} {
	var locks [dirtyWorkLockStripes]chan struct{}
	for i := range locks {
		locks[i] = make(chan struct{}, 1)
	}
	return locks
}()

// dirtyWorkBeforeLock and dirtyWorkBeforeRefUpdate are test seams. The lock
// hook runs before the checkout lock is taken. The ref hook runs after the
// snapshot matches and before update-ref, while the cleanup lock is held.
var (
	dirtyWorkBeforeLock        func()
	dirtyWorkBeforeRefUpdate   func()
	dirtyWorkAfterContentCheck func(string)
	dirtyWorkAfterBookkeeping  func(string)
)

func dirtyWorkLockIndex(worktree string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(canonicalPath(worktree)))
	return h.Sum32() % dirtyWorkLockStripes
}

func dirtyWorkCheckoutLocked(worktree string) bool {
	mu := dirtyWorkLocks[dirtyWorkLockIndex(worktree)]
	select {
	case mu <- struct{}{}:
		<-mu
		return false
	default:
		return true
	}
}

// WithDirtyWorkCheckouts serializes sweeping, snapshot, and removal of checkouts.
// Callers hold it across capture, the pre-removal recheck, and deletion so
// an older pass paused before publishing cannot replace the ref written by
// the pass that already deleted the checkout.
func WithDirtyWorkCheckouts(ctx context.Context, worktrees []string, fn func() error) error {
	if dirtyWorkBeforeLock != nil {
		dirtyWorkBeforeLock()
	}
	var selected [dirtyWorkLockStripes]bool
	for _, worktree := range worktrees {
		selected[dirtyWorkLockIndex(worktree)] = true
	}
	for index, acquire := range selected {
		if !acquire {
			continue
		}
		mu := dirtyWorkLocks[index]
		select {
		case mu <- struct{}{}:
			defer func() { <-mu }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func loadDirtyEntries(ctx context.Context, wt string, entries []dirtyEntry) ([]dirtyEntry, error) {
	root := canonicalPath(wt)
	for i := range entries {
		rel, err := cleanCheckoutRel(entries[i].rel)
		if err != nil {
			return nil, err
		}
		entries[i].rel = rel
		full := filepath.Join(wt, rel)
		if err := assertPathOwned(root, full); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrDirtyWorkRejected, err)
		}
		info, lerr := os.Lstat(full)
		switch {
		case lerr != nil && !errors.Is(lerr, os.ErrNotExist):
			return nil, fmt.Errorf("stat %s: %w", rel, lerr)
		case lerr == nil && !snapshotableFile(info):
			return nil, notSnapshotable(rel, info)
		}
		entries[i].kind = dirtyKind(entries[i].code)
	}
	return entries, nil
}

func snapshotableFile(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return info.Mode().IsRegular()
}

func notSnapshotable(rel string, info os.FileInfo) error {
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory and cannot be snapshotted as a file", ErrDirtyWorkRejected, rel)
	}
	return fmt.Errorf("%w: %s is not a regular file", ErrDirtyWorkRejected, rel)
}

func dirtyKind(code string) string {
	if code == "??" {
		return "untracked"
	}
	index := code[0] != ' ' && code[0] != '?'
	work := code[1] != ' ' && code[1] != '?'
	switch {
	case index && work:
		return "both"
	case index:
		return "staged"
	default:
		return "unstaged"
	}
}

func assertCheckoutOwned(ctx context.Context, gateDir, wt string) error {
	if strings.TrimSpace(gateDir) == "" {
		return fmt.Errorf("%w: snapshot repository is missing", ErrDirtyWorkRejected)
	}
	gateCommon, err := gitCommonDir(ctx, gateDir)
	if err != nil {
		return fmt.Errorf("snapshot repository: %w", err)
	}
	wtCommon, err := gitCommonDir(ctx, wt)
	if err != nil {
		return fmt.Errorf("checkout repository: %w", err)
	}
	if gateCommon != wtCommon {
		return fmt.Errorf("%w: worktree is not a checkout of this repository", ErrDirtyWorkRejected)
	}
	if pathWithin(canonicalPath(wt), gateCommon) {
		return fmt.Errorf("%w: snapshot repository is inside the checkout being removed", ErrDirtyWorkRejected)
	}
	return nil
}

func gitCommonDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return canonicalPath(out), nil
}

func assertPathOwned(root, full string) error {
	parent := filepath.Dir(full)
	resolvedParent, err := nearestExistingPath(parent)
	if err != nil {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	lexical := filepath.Join(resolvedParent, filepath.Base(full))
	if !pathWithin(root, lexical) {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil
	}
	if !pathWithin(root, target) {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	return nil
}

// nearestExistingPath resolves path, walking up to the nearest existing
// ancestor when a tracked directory has been deleted. The missing suffix is
// joined lexically onto that ancestor, so ownership is the ancestor's real
// location plus the deleted names, and a symlink ancestor still resolves.
func nearestExistingPath(path string) (string, error) {
	var missing []string
	cur := path
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			resolved, evalErr := filepath.EvalSymlinks(cur)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

func cleanCheckoutRel(rel string) (string, error) {
	if rel == "" || strings.ContainsAny(rel, "\x00\n\r\t") {
		return "", fmt.Errorf("%w: dirty path is not a single checkout-relative path", ErrDirtyWorkRejected)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: dirty path is absolute", ErrDirtyWorkRejected)
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: dirty path leaves the checkout", ErrDirtyWorkRejected)
	}
	return cleaned, nil
}

var errNotRegular = errors.New("not a regular file")

type preparedEntry struct {
	dirtyEntry
	mode string
	tmp  string
	link string
}

func worktreeDeleted(code string) bool {
	return len(code) == 2 && code[1] == 'D'
}

func worktreeChanged(code string) bool {
	return len(code) == 2 && code[1] != ' ' && code[1] != '?' && code[1] != 'D'
}

func entryHasBytes(ctx context.Context, wt string, entry dirtyEntry, full string) bool {
	if _, err := os.Lstat(full); err == nil {
		return true
	}
	if entry.code == "??" || entry.code[0] == 'D' || entry.code[0] == ' ' || entry.code[0] == '?' {
		return false
	}
	_, err := Run(ctx, wt, "cat-file", "-e", ":"+entry.rel)
	return err == nil
}

func secretPath(rel string) bool {
	slashed := filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(slashed))
	switch base {
	case ".env", ".npmrc", ".git-credentials", ".netrc", "_netrc", ".pypirc",
		"credentials", "credentials.json", "credentials.yml", "credentials.yaml",
		"secrets.json", "secrets.yaml", "secrets.yml",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}
	if strings.HasPrefix(base, ".env.") && base != ".env.example" && base != ".env.sample" && base != ".env.template" {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".p12", ".pfx":
		return true
	}
	for _, part := range strings.Split(slashed, "/") {
		switch strings.ToLower(part) {
		case ".ssh", ".aws", ".gnupg":
			return true
		}
	}
	return false
}

var privateKeyMarkers = [][]byte{
	[]byte("-----BEGIN PRIVATE KEY-----"),
	[]byte("-----BEGIN ENCRYPTED PRIVATE KEY-----"),
	[]byte("-----BEGIN RSA PRIVATE KEY-----"),
	[]byte("-----BEGIN OPENSSH PRIVATE KEY-----"),
	[]byte("-----BEGIN EC PRIVATE KEY-----"),
	[]byte("-----BEGIN DSA PRIVATE KEY-----"),
	[]byte("-----BEGIN PGP PRIVATE KEY BLOCK-----"),
}

const secretReadChunk = 32 * 1024

func secretMarkerOverlap() int {
	n := 0
	for _, marker := range privateKeyMarkers {
		if len(marker)-1 > n {
			n = len(marker) - 1
		}
	}
	return n
}

func bytesContainSecret(buf []byte) bool {
	for _, marker := range privateKeyMarkers {
		if bytes.Contains(buf, marker) {
			return true
		}
	}
	return false
}

func readerContainsSecret(ctx context.Context, r io.Reader) (bool, error) {
	return writeReaderScan(ctx, io.Discard, r)
}

// absorbSecretChunk reports whether tail+chunk holds a private-key marker and
// returns the overlap that the next chunk must be checked against. The file
// reader and the staged-blob scanner both use it, so a marker split across
// reads is classified the same way on either path.
func absorbSecretChunk(tail, chunk []byte) (next []byte, found bool) {
	if len(chunk) == 0 {
		return tail, false
	}
	window := make([]byte, 0, len(tail)+len(chunk))
	window = append(window, tail...)
	window = append(window, chunk...)
	if bytesContainSecret(window) {
		return nil, true
	}
	overlap := secretMarkerOverlap()
	if len(window) > overlap {
		return append([]byte(nil), window[len(window)-overlap:]...), false
	}
	return append([]byte(nil), window...), false
}

func writeReaderScan(ctx context.Context, dst io.Writer, r io.Reader) (bool, error) {
	buf := make([]byte, secretReadChunk)
	var tail []byte
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return false, werr
			}
			var found bool
			tail, found = absorbSecretChunk(tail, buf[:n])
			if found {
				return true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func prepareDirtyContent(ctx context.Context, wt, tmp string, entries []dirtyEntry) ([]preparedEntry, error) {
	cleaned, err := loadDirtyEntries(ctx, wt, entries)
	if err != nil {
		return nil, err
	}
	prepared := make([]preparedEntry, 0, len(cleaned))
	for _, entry := range cleaned {
		full := filepath.Join(wt, entry.rel)
		if secretPath(entry.rel) && entryHasBytes(ctx, wt, entry, full) {
			return nil, fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, entry.rel)
		}
		item := preparedEntry{dirtyEntry: entry}
		if _, err := os.Lstat(full); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if entry.code == "??" || worktreeChanged(entry.code) {
					return nil, fmt.Errorf("%w: %s disappeared while capturing dirty work", ErrDirtyWorkRejected, entry.rel)
				}
				prepared = append(prepared, item)
				continue
			}
			return nil, err
		}
		mode, tmpPath, link, err := stageSnapshot(ctx, tmp, entry.rel, full)
		if err != nil {
			return nil, err
		}
		item.mode = mode
		item.tmp = tmpPath
		item.link = link
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func stageSnapshot(ctx context.Context, tmpDir, rel, full string) (mode, tmpPath, link string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", "", err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", "", "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return "", "", "", err
		}
		if bytesContainSecret([]byte(target)) {
			return "", "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, rel)
		}
		return "120000", "", filepath.ToSlash(target), nil
	}
	if !info.Mode().IsRegular() {
		return "", "", "", notSnapshotable(rel, info)
	}
	f, err := openRegular(full)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return "", "", "", notSnapshotable(rel, info)
		}
		return "", "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", "", err
	}
	if !st.Mode().IsRegular() {
		return "", "", "", notSnapshotable(rel, st)
	}
	dst, err := os.CreateTemp(tmpDir, "blob-")
	if err != nil {
		return "", "", "", err
	}
	secret, copyErr := writeReaderScan(ctx, dst, f)
	closeErr := dst.Close()
	if copyErr != nil {
		return "", "", "", copyErr
	}
	if closeErr != nil {
		return "", "", "", closeErr
	}
	if secret {
		return "", "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, rel)
	}
	mode = "100644"
	if st.Mode()&0o111 != 0 {
		mode = "100755"
	}
	return mode, dst.Name(), "", nil
}

func rejectIndexIntent(ctx context.Context, wt string, env []string) error {
	visible, err := runInDirWithEnvRaw(ctx, wt, env, "diff", "--cached", "--raw", "-z", "--no-renames", "--ita-visible-in-index", "HEAD")
	if err != nil {
		return err
	}
	invisible, err := runInDirWithEnvRaw(ctx, wt, env, "diff", "--cached", "--raw", "-z", "--no-renames", "--ita-invisible-in-index", "HEAD")
	if err != nil {
		return err
	}
	if !bytes.Equal(visible, invisible) {
		return fmt.Errorf("%w: intent-to-add entries cannot be restored from snapshot trees", ErrDirtyWorkRejected)
	}
	return nil
}

func scanIndexBlobs(ctx context.Context, wt, indexFile string) error {
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	if err := rejectIndexIntent(ctx, wt, env); err != nil {
		return err
	}
	out, err := runInDirWithEnvRaw(ctx, wt, env, "ls-files", "--stage", "-z")
	if err != nil {
		return fmt.Errorf("read copied index: %w", err)
	}
	changed, err := runInDirWithEnvRaw(ctx, wt, env, "diff", "--cached", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return fmt.Errorf("read newly captured index paths: %w", err)
	}
	newMaterial := make(map[string]bool)
	for _, path := range bytes.Split(changed, []byte{0}) {
		if len(path) != 0 {
			newMaterial[filepath.FromSlash(string(path))] = true
		}
	}
	return walkIndexBlobs(out, func(mode, path, sha string) error {
		if secretPath(path) {
			return fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, path)
		}
		if !newMaterial[path] {
			return nil
		}
		if mode == "160000" {
			return fmt.Errorf("%w: changed submodule %s cannot be snapshotted", ErrDirtyWorkRejected, path)
		}
		found, err := gitStreamContainsSecret(ctx, wt, "cat-file", "blob", sha)
		if err != nil {
			return fmt.Errorf("read index %s: %w", path, err)
		}
		if found {
			return fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, path)
		}
		return nil
	})
}

func walkIndexBlobs(out []byte, fn func(mode, path, sha string) error) error {
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		tab := bytes.IndexByte(raw, '\t')
		if tab < 0 {
			return fmt.Errorf("%w: unreadable index entry", ErrDirtyWorkRejected)
		}
		fields := bytes.Fields(raw[:tab])
		if len(fields) != 3 || string(fields[2]) != "0" {
			return fmt.Errorf("%w: index entry is not a resolved blob", ErrDirtyWorkRejected)
		}
		path, err := cleanCheckoutRel(string(raw[tab+1:]))
		if err != nil {
			return err
		}
		if err := fn(string(fields[0]), path, string(fields[1])); err != nil {
			return err
		}
	}
	return nil
}

// errSecretScanStop ends the stdout copy once a private-key marker is in hand.
// The shell runner treats it as a copy failure and reaps git; the caller
// reports the marker instead of this error.
var errSecretScanStop = errors.New("secret scan stop")

// secretStreamScanner is the stdout sink for a git blob read. RunShellCommand
// owns the pipe: it resumes a Windows process that ConfigureShellCommand
// starts suspended, copies until git closes the stream, and reaps any
// descendant still holding that pipe when the leader exits. Reading the pipe
// to completion before Wait never observes that close, so the scan blocks
// until the test process is killed.
type secretStreamScanner struct {
	ctx   context.Context
	tail  []byte
	found bool
}

func (s *secretStreamScanner) Write(p []byte) (int, error) {
	var found bool
	s.tail, found = absorbSecretChunk(s.tail, p)
	if found {
		s.found = true
		return len(p), errSecretScanStop
	}
	if err := s.ctx.Err(); err != nil {
		return len(p), err
	}
	return len(p), nil
}

func gitStreamContainsSecret(ctx context.Context, dir string, args ...string) (bool, error) {
	scan := &secretStreamScanner{ctx: ctx}
	err := gitStreamTo(ctx, dir, nil, scan, args...)
	if scan.found {
		return true, nil
	}
	return false, err
}

func gitStreamTo(ctx context.Context, dir string, env []string, dst io.Writer, args ...string) error {
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(nonInteractiveEnvForContext(ctx, dir), env...)
	cmd.Stdout = dst
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	winproc.Harden(cmd)
	shellenv.ConfigureShellCommand(cmd)
	err := shellenv.RunShellCommand(cmd)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(strings.TrimSpace(stderr.String())))
	}
	return nil
}

func writeDirtySnapshot(ctx context.Context, req DirtyWorkRequest, ref string, entries []dirtyEntry) (*DirtyWorkSnapshot, error) {
	wt := req.Worktree
	config, err := readDirtyWorkConfig(ctx, wt)
	if err != nil {
		return nil, err
	}
	head, err := Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read checkout head: %w", err)
	}
	scratch, err := newDirtyWorkScratch(ctx)
	if err != nil {
		return nil, err
	}
	defer scratch.close(ctx)
	tmp := scratch.path

	prepared, err := prepareDirtyContent(ctx, wt, tmp, entries)
	if err != nil {
		return nil, err
	}
	indexPath, err := absoluteGitPath(ctx, wt, "index")
	if err != nil {
		return nil, err
	}
	indexCopy := filepath.Join(tmp, "index")
	if err := copyFile(indexCopy, indexPath); err != nil {
		return nil, err
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + indexCopy}
	if err := scanIndexBlobs(ctx, wt, indexCopy); err != nil {
		return nil, err
	}
	indexTree, err := RunWithEnv(ctx, wt, indexEnv, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write index tree: %w", err)
	}
	liveTree, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("read live index: %w", err)
	}
	if liveTree != indexTree {
		return nil, fmt.Errorf("index changed while capturing dirty work")
	}
	fileMode, err := checkoutFileMode(ctx, wt)
	if err != nil {
		return nil, err
	}
	if !fileMode {
		out, err := runInDirWithEnvRaw(ctx, wt, indexEnv, "ls-files", "--stage", "-z")
		if err != nil {
			return nil, err
		}
		modes := make(map[string]string)
		if err := walkIndexBlobs(out, func(mode, path, sha string) error {
			modes[path] = mode
			return nil
		}); err != nil {
			return nil, err
		}
		for i := range prepared {
			if prepared[i].mode == "100644" || prepared[i].mode == "100755" {
				prepared[i].mode = "100644"
				if prepared[i].code != "??" && modes[prepared[i].rel] == "100755" {
					prepared[i].mode = "100755"
				}
			}
		}
	}

	workTree, err := buildWorkTree(ctx, wt, tmp, indexCopy, prepared)
	if err != nil {
		return nil, err
	}
	liveAgain, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return nil, err
	}
	if liveAgain != indexTree {
		return nil, fmt.Errorf("capture changed the checkout index")
	}

	untrackedTree := ""
	untrackedCommit := ""
	ident := dirtyWorkIdentity()
	if hasUntracked(prepared) {
		untrackedTree, err = writeUntrackedTree(ctx, wt, tmp, prepared)
		if err != nil {
			return nil, err
		}
		untrackedCommit, err = RunWithEnv(ctx, wt, ident, "commit-tree", untrackedTree, "-m", "untracked files")
		if err != nil {
			return nil, fmt.Errorf("commit untracked tree: %w", err)
		}
	}
	indexCommit, err := RunWithEnv(ctx, wt, ident, "commit-tree", indexTree, "-p", head, "-m", "index")
	if err != nil {
		return nil, fmt.Errorf("commit index tree: %w", err)
	}
	message := dirtyWorkMessage(head, req.BaseSHA, indexTree, workTree, untrackedTree, config, dirtyEntries(prepared))
	msgPath := filepath.Join(tmp, "message")
	if err := os.WriteFile(msgPath, []byte(message), 0o644); err != nil {
		return nil, err
	}
	args := []string{"commit-tree", workTree, "-p", head, "-p", indexCommit}
	if untrackedCommit != "" {
		args = append(args, "-p", untrackedCommit)
	}
	args = append(args, "-F", msgPath)
	commit, err := RunWithEnv(ctx, wt, ident, args...)
	if err != nil {
		return nil, fmt.Errorf("commit dirty work snapshot: %w", err)
	}
	if err := verifyDirtySnapshot(ctx, wt, commit, head, indexCommit, untrackedCommit, indexTree, workTree, message); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := Run(ctx, req.GateDir, "cat-file", "-t", commit); err != nil {
		return nil, fmt.Errorf("snapshot is not in the gate repository: %w", err)
	}
	if err := recheckDirtyWork(ctx, wt, tmp, head, indexTree, workTree, untrackedTree, config, nil); err != nil {
		return nil, err
	}
	if dirtyWorkBeforeRefUpdate != nil {
		dirtyWorkBeforeRefUpdate()
	}
	if _, err := Run(ctx, req.GateDir, "update-ref", ref, commit); err != nil {
		return nil, fmt.Errorf("record dirty work snapshot: %w", err)
	}
	recorded, err := Run(ctx, req.GateDir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("read dirty work snapshot ref: %w", err)
	}
	if recorded != commit {
		return nil, fmt.Errorf("dirty work snapshot ref recorded %s, verified %s", recorded, commit)
	}
	body, err := Run(ctx, req.GateDir, "log", "-1", "--format=%B", ref)
	if err != nil {
		return nil, fmt.Errorf("read dirty work snapshot from the gate: %w", err)
	}
	if strings.TrimSpace(body) != strings.TrimSpace(message) {
		return nil, fmt.Errorf("gate snapshot message does not match the verified manifest")
	}
	base := req.BaseSHA
	if base == "" {
		base = "-"
	}
	return &DirtyWorkSnapshot{Ref: ref, Commit: commit, Head: head, Base: base}, nil
}

func dirtyEntries(prepared []preparedEntry) []dirtyEntry {
	entries := make([]dirtyEntry, len(prepared))
	for i := range prepared {
		entries[i] = prepared[i].dirtyEntry
	}
	return entries
}

func hasUntracked(prepared []preparedEntry) bool {
	for _, path := range prepared {
		if path.code == "??" {
			return true
		}
	}
	return false
}

func writeUntrackedTree(ctx context.Context, wt, tmp string, prepared []preparedEntry) (string, error) {
	index := filepath.Join(tmp, "untracked-index")
	if err := os.WriteFile(index, nil, 0o644); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := RunWithEnv(ctx, wt, env, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("prepare untracked index: %w", err)
	}
	info, err := indexInfo(ctx, wt, prepared, func(path preparedEntry) bool { return path.code == "??" })
	if err != nil {
		return "", err
	}
	if info != "" {
		if _, err := runInDirWithEnvAndInput(ctx, wt, env, info, "update-index", "-z", "--index-info"); err != nil {
			return "", fmt.Errorf("record untracked files: %w", err)
		}
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write untracked tree: %w", err)
	}
	return tree, nil
}

func buildWorkTree(ctx context.Context, wt, tmp, indexCopy string, prepared []preparedEntry) (string, error) {
	if !needsWorktreeApply(prepared) {
		tree, err := RunWithEnv(ctx, wt, []string{"GIT_INDEX_FILE=" + indexCopy}, "write-tree")
		if err != nil {
			return "", fmt.Errorf("write worktree tree: %w", err)
		}
		return tree, nil
	}
	workIndex := filepath.Join(tmp, "worktree-index")
	if err := copyFile(workIndex, indexCopy); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + workIndex}
	info, err := indexInfo(ctx, wt, prepared, func(path preparedEntry) bool {
		return path.code != "??" && worktreeChanged(path.code)
	})
	if err != nil {
		return "", err
	}
	if info != "" {
		if _, err := runInDirWithEnvAndInput(ctx, wt, env, info, "update-index", "-z", "--index-info"); err != nil {
			return "", fmt.Errorf("snapshot tracked worktree: %w", err)
		}
	}
	var removes []string
	for _, path := range prepared {
		if path.code != "??" && worktreeDeleted(path.code) {
			removes = append(removes, filepath.ToSlash(path.rel))
		}
	}
	if len(removes) > 0 {
		args := append([]string{"update-index", "--force-remove", "--"}, removes...)
		if _, err := RunWithEnv(ctx, wt, env, args...); err != nil {
			return "", fmt.Errorf("snapshot tracked deletion: %w", err)
		}
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write worktree tree: %w", err)
	}
	return tree, nil
}

func needsWorktreeApply(prepared []preparedEntry) bool {
	for _, path := range prepared {
		if path.code == "??" {
			continue
		}
		if worktreeDeleted(path.code) || worktreeChanged(path.code) {
			return true
		}
	}
	return false
}

func indexInfo(ctx context.Context, wt string, prepared []preparedEntry, include func(preparedEntry) bool) (string, error) {
	var info strings.Builder
	for _, path := range prepared {
		if !include(path) {
			continue
		}
		blob, err := hashPrepared(ctx, wt, path)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path.rel, err)
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", path.mode, blob, filepath.ToSlash(path.rel))
	}
	return info.String(), nil
}

func hashPrepared(ctx context.Context, wt string, path preparedEntry) (string, error) {
	args := []string{"hash-object", "-w", "--no-filters"}
	if path.mode == "120000" {
		args = append(args, "--stdin")
		return runInDirWithEnvAndInput(ctx, wt, nil, path.link, args...)
	}
	if path.tmp == "" {
		return "", fmt.Errorf("%w: %s has no staged bytes", ErrDirtyWorkRejected, path.rel)
	}
	args = append(args, "--", path.tmp)
	return Run(ctx, wt, args...)
}

func recheckDirtyWork(ctx context.Context, wt, tmp, head, indexTree, workTree, untrackedTree string, config dirtyWorkConfig, observed map[string]os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attributeFile, err := Run(ctx, wt, "config", "--path", "--default", "", "--get", "core.attributesfile")
	if err != nil {
		return err
	}
	if attributeFile != "" {
		return fmt.Errorf("%w: a custom attributes file prevents restore verification", ErrDirtyWorkRejected)
	}
	if err := rejectIndexIntent(ctx, wt, nil); err != nil {
		return err
	}
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
		return err
	}
	fileMode, err := checkoutFileMode(ctx, wt)
	if err != nil {
		return err
	}
	current, err := Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("read checkout head: %w", err)
	}
	if current != head {
		return fmt.Errorf("checkout HEAD changed while capturing dirty work")
	}
	live, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return fmt.Errorf("read live index: %w", err)
	}
	if live != indexTree {
		return fmt.Errorf("index changed while capturing dirty work")
	}
	index := filepath.Join(tmp, "equal-index")
	if err := os.WriteFile(index, nil, 0o644); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := RunWithEnv(ctx, wt, env, "read-tree", workTree); err != nil {
		return fmt.Errorf("read tracked snapshot: %w", err)
	}
	trackedBlobs, err := runInDirWithEnvRaw(ctx, wt, env, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	if untrackedTree != "" {
		info, err := untrackedIndexInfo(ctx, wt, untrackedTree)
		if err != nil {
			return err
		}
		if len(info) > 0 {
			if _, err := runInDirWithEnvAndInput(ctx, wt, env, string(info), "update-index", "-z", "--index-info"); err != nil {
				return fmt.Errorf("read untracked snapshot: %w", err)
			}
		}
	}
	blobs, err := runInDirWithEnvRaw(ctx, wt, env, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	if observed == nil {
		observed = make(map[string]os.FileInfo)
	}
	gitlinks := make(map[string]bool)
	if err := walkIndexBlobs(blobs, func(mode, path, sha string) error {
		full := filepath.Join(wt, path)
		gitlinks[path] = mode == "160000"
		info, err := os.Lstat(full)
		if err != nil && !(mode == "160000" && errors.Is(err, os.ErrNotExist)) {
			return err
		}
		observed[full] = info
		return nil
	}); err != nil {
		return err
	}
	for _, tree := range []string{head, indexTree} {
		paths, err := RunRaw(ctx, wt, "ls-tree", "-r", "--name-only", "-z", tree)
		if err != nil {
			return err
		}
		for _, path := range bytes.Split(paths, []byte{0}) {
			if len(path) == 0 {
				continue
			}
			rel, err := cleanCheckoutRel(string(path))
			if err != nil {
				return err
			}
			full := filepath.Join(wt, rel)
			if _, ok := observed[full]; !ok {
				observed[full] = nil
			}
		}
	}
	preserveTracked := make(map[string]bool)
	for _, tree := range []string{indexTree, workTree} {
		changed, err := RunRaw(ctx, wt, "diff-tree", "-r", "--name-only", "-z", "--no-renames", head, tree)
		if err != nil {
			return err
		}
		for _, path := range bytes.Split(changed, []byte{0}) {
			if len(path) != 0 {
				preserveTracked[filepath.FromSlash(string(path))] = true
			}
		}
	}
	preserveAll := make(map[string]bool)
	for path := range preserveTracked {
		preserveAll[path] = true
	}
	if untrackedTree != "" {
		paths, err := RunRaw(ctx, wt, "ls-tree", "-r", "--name-only", "-z", untrackedTree)
		if err != nil {
			return err
		}
		for _, path := range bytes.Split(paths, []byte{0}) {
			if len(path) != 0 {
				preserveAll[filepath.FromSlash(string(path))] = true
			}
		}
	}
	ignoredAttributes, err := RunRaw(ctx, wt, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", ".gitattributes", ":(glob)**/.gitattributes")
	if err != nil {
		return err
	}
	attributesChanged := len(ignoredAttributes) != 0
	for path := range preserveAll {
		if filepath.Base(path) == ".gitattributes" {
			attributesChanged = true
		}
	}
	if attributesChanged {
		if err := walkIndexBlobs(trackedBlobs, func(mode, path, sha string) error {
			preserveTracked[path] = true
			return nil
		}); err != nil {
			return err
		}
		if err := walkIndexBlobs(blobs, func(mode, path, sha string) error {
			preserveAll[path] = true
			return nil
		}); err != nil {
			return err
		}
	}
	headTree, err := Run(ctx, wt, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return err
	}
	seenTrees := make(map[string]bool)
	for _, tree := range []string{headTree, indexTree, workTree} {
		if seenTrees[tree] {
			continue
		}
		seenTrees[tree] = true
		if err := verifyRestoreConversions(ctx, wt, tmp, tree, trackedBlobs, preserveTracked, config); err != nil {
			return err
		}
	}
	if untrackedTree != "" {
		if err := verifyRestoreConversions(ctx, wt, tmp, workTree, blobs, preserveAll, config); err != nil {
			return err
		}
		combined, err := RunWithEnv(ctx, wt, env, "write-tree")
		if err != nil {
			return err
		}
		if err := verifyRestoreConversions(ctx, wt, tmp, combined, blobs, preserveAll, config); err != nil {
			return err
		}
	}
	if err := walkIndexBlobs(blobs, func(mode, path, sha string) error {
		differs, _, err := trackedPathDiffers(ctx, wt, path, mode, sha, fileMode, observed)
		if err != nil {
			return err
		}
		if differs {
			return fmt.Errorf("%w: snapshot bytes differ from %s", ErrDirtyWorkRejected, path)
		}
		return nil
	}); err != nil {
		return err
	}
	if dirtyWorkAfterContentCheck != nil {
		dirtyWorkAfterContentCheck(wt)
	}
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
		return err
	}
	out, err := runInDirWithEnvAndInputRaw(ctx, wt, env, "", gitStatusArgs()...)
	if err != nil {
		return err
	}
	if err := statusMatchesSnapshot(out, gitlinks); err != nil {
		return err
	}
	activeConfig, err := readDirtyWorkConfig(ctx, wt)
	if err != nil {
		return err
	}
	activeFileMode, err := checkoutFileMode(ctx, wt)
	if err != nil {
		return err
	}
	if activeConfig != config || activeFileMode != fileMode {
		return fmt.Errorf("%w: checkout configuration changed during verification", ErrDirtyWorkRejected)
	}
	if err := rejectIndexIntent(ctx, wt, nil); err != nil {
		return err
	}
	live, err = Run(ctx, wt, "write-tree")
	if err != nil {
		return err
	}
	if live != indexTree {
		return fmt.Errorf("%w: index changed after content verification", ErrDirtyWorkRejected)
	}
	current, err = Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if current != head {
		return fmt.Errorf("%w: HEAD changed after content verification", ErrDirtyWorkRejected)
	}
	if dirtyWorkAfterBookkeeping != nil {
		dirtyWorkAfterBookkeeping(wt)
	}
	for path, before := range observed {
		after, err := os.Lstat(path)
		if before == nil && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || before == nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("%w: observed path changed: %s", ErrDirtyWorkRejected, path)
		}
	}
	others, err := runInDirWithEnvRaw(ctx, wt, env, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if len(others) != 0 {
		return fmt.Errorf("%w: checkout file membership changed", ErrDirtyWorkRejected)
	}
	return ctx.Err()
}

func verifyRestoreConversions(ctx context.Context, wt, tmp, tree string, blobs []byte, paths map[string]bool, config dirtyWorkConfig) error {
	if len(paths) == 0 {
		return nil
	}
	root, err := os.MkdirTemp(tmp, "restore-attributes-")
	if err != nil {
		return err
	}
	gitDir, err := Run(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	env := []string{"GIT_DIR=" + gitDir, "GIT_INDEX_FILE=" + filepath.Join(tmp, "restore-index"), "GIT_WORK_TREE=" + root}
	if _, err := RunWithEnv(ctx, root, env, "read-tree", tree); err != nil {
		return err
	}
	stageBlobs, err := runInDirWithEnvRaw(ctx, root, env, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	protected := make(map[string]bool)
	if err := walkIndexBlobs(blobs, func(mode, path, sha string) error {
		protected[path] = true
		return nil
	}); err != nil {
		return err
	}
	if err := walkIndexBlobs(stageBlobs, func(mode, path, sha string) error {
		if filepath.Base(path) != ".gitattributes" || (mode != "100644" && mode != "100755") {
			return nil
		}
		protected[path] = true
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		file, err := os.Create(full)
		if err != nil {
			return err
		}
		err = gitStreamTo(ctx, root, env, file, "cat-file", "blob", sha)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}); err != nil {
		return err
	}
	var input strings.Builder
	for path := range protected {
		input.WriteString(filepath.ToSlash(path))
		input.WriteByte(0)
	}
	attrArgs := []string{"-z", "--stdin", "--all"}
	cachedArgs := append([]string{"check-attr", "--cached"}, attrArgs...)
	attrs, err := runInDirWithEnvAndInputRaw(ctx, root, env, input.String(), cachedArgs...)
	if err != nil {
		return err
	}
	parseRules := func(out []byte) (map[[2]string]string, error) {
		rules := make(map[[2]string]string)
		if len(out) == 0 {
			return rules, nil
		}
		fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
		if len(fields)%3 != 0 {
			return nil, fmt.Errorf("%w: unreadable restore attributes", ErrDirtyWorkRejected)
		}
		for i := 0; i < len(fields); i += 3 {
			rules[[2]string{string(fields[i]), string(fields[i+1])}] = string(fields[i+2])
		}
		return rules, nil
	}
	cachedRules, err := parseRules(attrs)
	if err != nil {
		return err
	}
	for key, value := range cachedRules {
		path := filepath.FromSlash(key[0])
		if key[1] == "filter" && (paths[path] || filepath.Base(path) == ".gitattributes") && value != "unspecified" && value != "unset" {
			return fmt.Errorf("%w: external conversion for %s cannot be verified", ErrDirtyWorkRejected, key[0])
		}
	}
	convertedRoot, err := os.MkdirTemp(tmp, "checkout-attributes-")
	if err != nil {
		return err
	}
	if err := walkIndexBlobs(stageBlobs, func(mode, path, sha string) error {
		if filepath.Base(path) != ".gitattributes" || (mode != "100644" && mode != "100755") {
			return nil
		}
		full := filepath.Join(convertedRoot, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		file, err := os.Create(full)
		if err != nil {
			return err
		}
		err = gitStreamTo(ctx, root, env, file, config.args("cat-file", "--filters", "--path="+filepath.ToSlash(path), sha)...)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}); err != nil {
		return err
	}
	root = convertedRoot
	env = append(env, "GIT_WORK_TREE="+root)
	checkoutArgs := append([]string{"check-attr"}, attrArgs...)
	checkoutAttrs, err := runInDirWithEnvAndInputRaw(ctx, root, env, input.String(), checkoutArgs...)
	if err != nil {
		return err
	}
	checkoutRules, err := parseRules(checkoutAttrs)
	if err != nil {
		return err
	}
	if !maps.Equal(cachedRules, checkoutRules) {
		return fmt.Errorf("%w: checked-out attributes change conversion rules", ErrDirtyWorkRejected)
	}
	check := func(mode, path, sha, expectedPath string) error {
		if mode == "120000" || mode == "160000" {
			return nil
		}
		restored := sha256.New()
		err := gitStreamTo(ctx, root, env, restored, config.args("cat-file", "--filters", "--path="+filepath.ToSlash(path), sha)...)
		if err != nil {
			return err
		}
		expected, err := hashRegularBytes(ctx, expectedPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(restored.Sum(nil), expected) {
			return fmt.Errorf("%w: Git conversion changes restored bytes for %s", ErrDirtyWorkRejected, path)
		}
		return nil
	}
	if err := walkIndexBlobs(stageBlobs, func(mode, path, sha string) error {
		if filepath.Base(path) == ".gitattributes" {
			return check(mode, path, sha, filepath.Join(root, path))
		}
		return nil
	}); err != nil {
		return err
	}
	return walkIndexBlobs(blobs, func(mode, path, sha string) error {
		if !paths[path] {
			return nil
		}
		return check(mode, path, sha, filepath.Join(wt, path))
	})
}

func untrackedIndexInfo(ctx context.Context, wt, tree string) ([]byte, error) {
	out, err := RunRaw(ctx, wt, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, fmt.Errorf("list untracked snapshot: %w", err)
	}
	var info bytes.Buffer
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		tab := bytes.IndexByte(raw, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("%w: unreadable untracked snapshot", ErrDirtyWorkRejected)
		}
		fields := bytes.Fields(raw[:tab])
		if len(fields) < 3 {
			return nil, fmt.Errorf("%w: unreadable untracked snapshot", ErrDirtyWorkRejected)
		}
		info.Write(fields[0])
		info.WriteByte(' ')
		info.Write(fields[2])
		info.WriteByte('\t')
		info.Write(raw[tab+1:])
		info.WriteByte(0)
	}
	return info.Bytes(), nil
}

func statusMatchesSnapshot(out []byte, gitlinks map[string]bool) error {
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if len(raw) < 4 || raw[2] != ' ' {
			return fmt.Errorf("%w: unreadable status entry", ErrDirtyWorkRejected)
		}
		if raw[0] == '?' || (raw[1] != ' ' && gitlinks[filepath.FromSlash(string(raw[3:]))]) {
			return fmt.Errorf("checkout changed while capturing dirty work")
		}
	}
	return nil
}

func rejectNonSnapshotable(ctx context.Context, wt string) error {
	prefixes, err := ignoredDirPrefixes(ctx, wt)
	if err != nil {
		return err
	}
	var odd []string
	err = filepath.WalkDir(wt, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(rel)
		if hasIgnoredPrefix(prefixes, slash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if snapshotableFile(info) {
			return nil
		}
		odd = append(odd, slash)
		return nil
	})
	if err != nil {
		return err
	}
	if len(odd) == 0 {
		return nil
	}
	ignored, err := ignoredPathSet(ctx, wt, odd)
	if err != nil {
		return err
	}
	for _, rel := range odd {
		if _, ok := ignored[rel]; ok {
			continue
		}
		info, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		return notSnapshotable(rel, info)
	}
	return nil
}

func ignoredDirPrefixes(ctx context.Context, wt string) ([]string, error) {
	out, err := RunRaw(ctx, wt, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	var prefixes []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 || raw[len(raw)-1] != '/' {
			continue
		}
		prefixes = append(prefixes, filepath.ToSlash(string(raw[:len(raw)-1])))
	}
	return prefixes, nil
}

func hasIgnoredPrefix(prefixes []string, slash string) bool {
	for _, prefix := range prefixes {
		if slash == prefix || strings.HasPrefix(slash, prefix+"/") {
			return true
		}
	}
	return false
}

func ignoredPathSet(ctx context.Context, wt string, paths []string) (map[string]struct{}, error) {
	var input bytes.Buffer
	for _, rel := range paths {
		input.WriteString(rel)
		input.WriteByte(0)
	}
	out, err := runInDirWithEnvAndInputRaw(ctx, wt, nil, input.String(), "check-ignore", "-z", "--stdin")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return map[string]struct{}{}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("check ignored paths: %w", err)
	}
	ignored := make(map[string]struct{})
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		ignored[filepath.ToSlash(string(raw))] = struct{}{}
	}
	return ignored, nil
}

func assertCheckoutClean(ctx context.Context, wt string, observed map[string]os.FileInfo) error {
	config, err := readDirtyWorkConfig(ctx, wt)
	if err != nil {
		return err
	}
	head, err := Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	tree, err := Run(ctx, wt, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return err
	}
	scratch, err := newDirtyWorkScratch(ctx)
	if err != nil {
		return err
	}
	defer scratch.close(ctx)
	tmp := scratch.path
	return recheckDirtyWork(ctx, wt, tmp, head, tree, tree, "", config, observed)
}

func readSnapshotTrees(ctx context.Context, wt, commit string) (head, indexTree, workTree, untrackedTree string, err error) {
	line, err := Run(ctx, wt, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot parents: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || len(fields) > 4 {
		return "", "", "", "", fmt.Errorf("dirty work snapshot %s has %d parents", commit, len(fields)-1)
	}
	head = fields[1]
	indexTree, err = Run(ctx, wt, "rev-parse", "--verify", fields[2]+"^{tree}")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot index: %w", err)
	}
	workTree, err = Run(ctx, wt, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot worktree: %w", err)
	}
	if len(fields) == 4 {
		untrackedTree, err = Run(ctx, wt, "rev-parse", "--verify", fields[3]+"^{tree}")
		if err != nil {
			return "", "", "", "", fmt.Errorf("read snapshot untracked tree: %w", err)
		}
	}
	return head, indexTree, workTree, untrackedTree, nil
}

func verifyDirtySnapshot(ctx context.Context, wt, commit, head, indexCommit, untrackedCommit, indexTree, workTree, message string) error {
	parents := []struct {
		spec string
		want string
	}{
		{commit + "^1", head},
		{commit + "^2", indexCommit},
	}
	if untrackedCommit != "" {
		parents = append(parents, struct {
			spec string
			want string
		}{commit + "^3", untrackedCommit})
	}
	for _, parent := range parents {
		got, err := Run(ctx, wt, "rev-parse", "--verify", parent.spec)
		if err != nil {
			return fmt.Errorf("snapshot parent %s: %w", parent.spec, err)
		}
		if got != parent.want {
			return fmt.Errorf("snapshot parent %s is %s, want %s", parent.spec, got, parent.want)
		}
	}
	if untrackedCommit == "" {
		if _, err := Run(ctx, wt, "rev-parse", "--verify", "--quiet", commit+"^3"); err == nil {
			return fmt.Errorf("snapshot has an unexpected untracked parent")
		}
	}
	gotTree, err := Run(ctx, wt, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return err
	}
	if gotTree != workTree {
		return fmt.Errorf("snapshot worktree tree is %s, want %s", gotTree, workTree)
	}
	gotIndex, err := Run(ctx, wt, "rev-parse", "--verify", indexCommit+"^{tree}")
	if err != nil {
		return err
	}
	if gotIndex != indexTree {
		return fmt.Errorf("snapshot index tree is %s, want %s", gotIndex, indexTree)
	}
	body, err := Run(ctx, wt, "log", "-1", "--format=%B", commit)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) != strings.TrimSpace(message) {
		return fmt.Errorf("snapshot message does not match the verified manifest")
	}
	return nil
}

func dirtyWorkMessage(head, base, indexTree, workTree, untrackedTree string, config dirtyWorkConfig, paths []dirtyEntry) string {
	if base == "" {
		base = "-"
	}
	if untrackedTree == "" {
		untrackedTree = "-"
	}
	var b strings.Builder
	b.WriteString("no-mistakes dirty-work snapshot\n\n")
	b.WriteString("schema: 1\n")
	fmt.Fprintf(&b, "head: %s\n", head)
	fmt.Fprintf(&b, "base: %s\n", base)
	fmt.Fprintf(&b, "index: %s\n", indexTree)
	fmt.Fprintf(&b, "worktree: %s\n", workTree)
	fmt.Fprintf(&b, "untracked: %s\n", untrackedTree)
	fmt.Fprintf(&b, "restore-core-autocrlf: %s\n", config.autoCRLF)
	fmt.Fprintf(&b, "restore-core-eol: %s\n", config.eol)
	for _, path := range paths {
		fmt.Fprintf(&b, "path: %s %s\n", path.kind, filepath.ToSlash(path.rel))
	}
	return b.String()
}

func dirtyWorkIdentity() []string {
	return []string{
		"GIT_AUTHOR_NAME=no-mistakes",
		"GIT_AUTHOR_EMAIL=no-mistakes@localhost",
		"GIT_COMMITTER_NAME=no-mistakes",
		"GIT_COMMITTER_EMAIL=no-mistakes@localhost",
	}
}

func absoluteGitPath(ctx context.Context, dir, name string) (string, error) {
	p, err := Run(ctx, dir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
