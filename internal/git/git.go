package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// EmptyTreeSHA is the well-known SHA of an empty tree in git.
// Used as a base when there is no prior commit to diff against.
const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// IsZeroSHA returns true if the SHA is the null/zero ref that git uses for
// new or deleted branches (40 zeros).
func IsZeroSHA(sha string) bool {
	return sha == "0000000000000000000000000000000000000000"
}

// Run executes a git command in the given directory and returns trimmed stdout.
// Returns an error that includes the command and stderr on failure.
//
// When dir is itself a bare repository (a gate repo), the repo is named
// explicitly via --git-dir instead of relying on cwd-based discovery, which
// safe.bareRepository=explicit forbids. Agent harnesses (e.g. Claude Code)
// and hardened CI inject that setting, so gate operations must never depend
// on discovering a bare repo from the working directory (issue #362).
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := RunRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// RunRaw executes a git command and returns stdout without modifying its bytes.
func RunRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if isBareGitDir(dir) {
		return runInDirWithEnvRaw(ctx, dir, nil, append([]string{"--git-dir=" + dir}, args...)...)
	}
	return runInDirWithEnvRaw(ctx, dir, nil, args...)
}

// RunBare executes Git against exactly bareDir. Unlike Run, it never falls
// back to cwd-based repository discovery when bareDir is malformed. Gate
// recovery uses this after structural validation so an invalid directory under
// NM_HOME cannot discover or mutate an ancestor worktree.
func RunBare(ctx context.Context, bareDir string, args ...string) (string, error) {
	if bareDir == "" {
		return "", fmt.Errorf("bare git directory is empty")
	}
	return runInDir(ctx, bareDir, append([]string{"--git-dir=" + bareDir}, args...)...)
}

// RunWithEnv is Run with extra KEY=VALUE entries appended to the git
// environment. Later entries win, so a caller can override anything
// NonInteractiveEnv sets. It exists for plumbing that is configured only
// through the environment - GIT_INDEX_FILE for a scratch index, the
// GIT_AUTHOR_*/GIT_COMMITTER_* identity for commit-tree - and carries the same
// bare-repository handling as Run.
func RunWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		return runInDirWithEnv(ctx, dir, extraEnv, append([]string{"--git-dir=" + dir}, args...)...)
	}
	return runInDirWithEnv(ctx, dir, extraEnv, args...)
}

func runInDir(ctx context.Context, dir string, args ...string) (string, error) {
	return runInDirWithEnv(ctx, dir, nil, args...)
}

func runInDirWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	out, err := runInDirWithEnvRaw(ctx, dir, extraEnv, args...)
	return strings.TrimSpace(string(out)), err
}

func runInDirWithEnvRaw(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(nonInteractiveEnvForContext(ctx, dir), extraEnv...)
	winproc.Harden(cmd)
	// OutputShellCommand captures stdout only, so unlike cmd.Output it never
	// fills ExitError.Stderr. Capture stderr explicitly or the git error text
	// below silently becomes empty.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	shellenv.ConfigureShellCommand(cmd)
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%v)", ctxErr, err)
		}
		return nil, fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}

// ValidateBareRepository verifies both the filesystem shape and Git's own bare
// repository classification. The Git query is explicitly scoped with
// --git-dir, so validation itself cannot discover an ancestor repository.
func ValidateBareRepository(ctx context.Context, bareDir string) error {
	if !isBareGitDir(bareDir) {
		return fmt.Errorf("not a structurally valid bare repository: %s", bareDir)
	}
	out, err := RunBare(ctx, bareDir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("validate bare repository: %w", err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("repository is not bare: %s", bareDir)
	}
	return nil
}

// LooksLikeBareRepository performs the non-mutating structural half of bare
// repository validation. Call ValidateBareRepository before mutating an
// unstamped directory discovered from the filesystem.
func LooksLikeBareRepository(dir string) bool {
	return isBareGitDir(dir)
}

// isBareGitDir reports whether dir is itself a git directory (a bare repo),
// as opposed to a working tree or linked worktree, which carry a .git entry
// and keep using normal discovery. The check mirrors git's own git-dir
// heuristic: a HEAD file plus an objects directory.
func isBareGitDir(dir string) bool {
	if dir == "" {
		return false
	}
	root, err := os.Lstat(dir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && fi.IsDir()
}

// InitBare creates a new bare git repository at the given path.
func InitBare(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", path)
	cmd.Env = nonInteractiveEnvForContext(ctx, "")
	winproc.Harden(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddRemote adds a named remote to the repo at dir.
func AddRemote(ctx context.Context, dir, name, url string) error {
	_, err := Run(ctx, dir, "remote", "add", name, url)
	return err
}

// EnsureRemote sets the named remote to url, adding it when absent and
// updating its URL when it already exists. Idempotent, so it is safe to call
// when repairing or re-running an init.
func EnsureRemote(ctx context.Context, dir, name, url string) error {
	if _, err := GetRemoteURL(ctx, dir, name); err == nil {
		_, err := Run(ctx, dir, "remote", "set-url", name, url)
		return err
	}
	return AddRemote(ctx, dir, name, url)
}

// RemoveRemote removes a named remote from the repo at dir.
func RemoveRemote(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "remote", "remove", name)
	return err
}

// GetRemoteURL returns the URL of a named remote.
func GetRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "remote", "get-url", name)
}

// GetConfiguredRemoteURL returns the literal remote URL from git config,
// without applying url.*.insteadOf rewrites.
func GetConfiguredRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "config", "--get", "remote."+name+".url")
}

// GetConfiguredRemoteURLs returns every literal URL configured for a remote.
// Callers that require an authoritative source can reject zero or multiple
// values rather than letting git silently select one.
func GetConfiguredRemoteURLs(ctx context.Context, dir, name string) ([]string, error) {
	out, err := Run(ctx, dir, "config", "--null", "--get-all", "remote."+name+".url")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// HasRemote reports whether a remote named name is configured in the repo at
// dir, returning an error if the remote list cannot be read.
func HasRemote(ctx context.Context, dir, name string) (bool, error) {
	out, err := Run(ctx, dir, "remote")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// FindGitRoot walks up from path to find the git repository root.
// Resolves symlinks for consistency on macOS (e.g. /tmp -> /private/tmp).
func FindGitRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = abs
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	root := strings.TrimSpace(string(out))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root, nil
	}
	return resolved, nil
}

// FindMainRepoRoot returns the root of the main working tree for a git
// repository. Three layouts are supported:
//
//  1. A regular repository or a linked worktree: the git common dir is
//     <root>/.git, so the main working tree is filepath.Dir(commonDir).
//  2. An absorbed submodule (including nested .../modules/a/modules/b):
//     the git common dir lives under the superproject's .git/modules/...
//     and is detached from its working tree. Git writes core.worktree
//     when it absorbs a submodule, pointing at the working tree whose
//     remote.origin.url is the submodule's own origin (which is what
//     callers like init and eject need).
//  3. Exotic GIT_DIR layouts without a core.worktree: fall back to
//     `git rev-parse --show-toplevel` from the original path, the same
//     answer FindGitRoot returns.
//
// In every branch the returned path is run through filepath.EvalSymlinks
// when possible so callers can compare it against other symlink-resolved
// paths (notably on macOS, where /tmp and /private/tmp refer to the same
// directory). Symlink resolution failures fall back to the unresolved
// path, matching the historical behavior.
func FindMainRepoRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	// Resolve the git common dir.
	commonDirCmd := exec.Command("git", "rev-parse", "--git-common-dir")
	commonDirCmd.Dir = abs
	winproc.Harden(commonDirCmd)
	commonDirOut, err := commonDirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	commonDir := strings.TrimSpace(string(commonDirOut))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}

	// Branch 1: regular repo or linked worktree. Linked worktrees share
	// the main repo's <root>/.git, so the common dir's basename is still
	// ".git" and its parent is the main working tree.
	if filepath.Base(commonDir) == ".git" {
		return resolveMainRoot(filepath.Dir(commonDir))
	}

	// Branch 2: detached git dir (absorbed submodule). Ask the git dir
	// itself for its core.worktree, which git writes when it absorbs a
	// submodule's git dir. The value is typically relative (e.g.
	// "../../../sub"); resolve it against the common dir.
	worktreeCmd := exec.Command("git", "--git-dir", commonDir, "config", "--get", "core.worktree")
	winproc.Harden(worktreeCmd)
	if worktreeOut, err := worktreeCmd.Output(); err == nil {
		worktree := strings.TrimSpace(string(worktreeOut))
		if worktree != "" {
			if !filepath.IsAbs(worktree) {
				worktree = filepath.Join(commonDir, worktree)
			}
			return resolveMainRoot(worktree)
		}
	}

	// Branch 3: exotic GIT_DIR without a usable core.worktree. Defer to
	// `git rev-parse --show-toplevel` from the original path.
	topCmd := exec.Command("git", "rev-parse", "--show-toplevel")
	topCmd.Dir = abs
	winproc.Harden(topCmd)
	topOut, err := topCmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	return resolveMainRoot(strings.TrimSpace(string(topOut)))
}

// resolveMainRoot applies filepath.EvalSymlinks to path, falling back to
// the unresolved path when symlink resolution fails.
func resolveMainRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, nil
	}
	return resolved, nil
}

// Diff returns the unified diff between two commits.
func Diff(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "diff", base+".."+head)
}

// DiffNameOnly returns the list of files changed between base and head.
// Output is split on newlines with empty entries removed.
func DiffNameOnly(ctx context.Context, dir, base, head string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", base+".."+head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

// DiffStat returns the bounded size of the diff between base and head: the
// number of changed files and the net changed lines (insertions + deletions)
// from `git diff --numstat`. Binary files (numstat "-") contribute a changed
// file but no line count. It carries no paths or content - just two counts.
func DiffStat(ctx context.Context, dir, base, head string) (files, lines int, err error) {
	out, err := Run(ctx, dir, "diff", "--numstat", base+".."+head)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		files++
		added, aerr := strconv.Atoi(fields[0])
		deleted, derr := strconv.Atoi(fields[1])
		if aerr == nil {
			lines += added
		}
		if derr == nil {
			lines += deleted
		}
	}
	return files, lines, nil
}

// CommitTime returns the committer timestamp for a SHA in UTC.
func CommitTime(ctx context.Context, dir, sha string) (time.Time, error) {
	out, err := Run(ctx, dir, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit time %q: %w", out, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// CommitAuthorEmail returns the author email for a SHA.
func CommitAuthorEmail(ctx context.Context, dir, sha string) (string, error) {
	return Run(ctx, dir, "show", "-s", "--format=%ae", sha)
}

// DiffHead returns the unified diff between HEAD and the working tree
// (both staged and unstaged changes).
func DiffHead(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "diff", "HEAD")
}

// Log returns oneline log entries between two commits.
func Log(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "log", "--oneline", base+".."+head)
}

// HeadSHA returns the full SHA of HEAD.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "HEAD")
}

// CurrentBranch returns the current branch name.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// IsDetachedHEAD reports whether the working tree is in a detached-HEAD state
// (HEAD points at a commit rather than a branch ref). Uses `git symbolic-ref`
// which fails cleanly when HEAD is not a symbolic ref.
func IsDetachedHEAD(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = dir
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// Exit 1 means HEAD is not a symbolic ref — detached.
			if ee.ExitCode() == 1 {
				return true, nil
			}
		}
		return false, fmt.Errorf("git symbolic-ref: %w", err)
	}
	return false, nil
}

// DefaultBranch queries a remote to determine its default branch name.
// Uses git ls-remote --symref to read the remote's HEAD symref.
// Falls back to "main" if detection fails (e.g. empty remote, unreachable).
func DefaultBranch(ctx context.Context, dir, remote string) string {
	out, err := Run(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "main"
	}
	// Output format: "ref: refs/heads/main\tHEAD\n<sha>\tHEAD\n"
	// Fields splits: ["ref:", "refs/heads/main", "HEAD"]
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strings.TrimPrefix(parts[1], "refs/heads/")
			}
		}
	}
	return "main"
}

// FetchRemoteBranch fetches a single branch into a remote-tracking ref.
// Uses a force-update refspec (+) so non-fast-forward updates (e.g. after
// a force push on the remote) are accepted instead of silently rejected.
func FetchRemoteBranch(ctx context.Context, dir, remote, branch string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

func FetchRemoteBranchToRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

// FetchRemoteBranchToPrivateRef fetches one branch into a caller-owned private
// ref without touching FETCH_HEAD or ordinary remote-tracking refs.
func FetchRemoteBranchToPrivateRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, refspec)
	return err
}

// FetchRemoteRef imports the object named by one exact remote ref without
// touching FETCH_HEAD or any caller-owned ref. The temporary ref is private to
// this operation, allowing the caller to publish the verified object with its
// own create-only or compare-and-swap policy.
func FetchRemoteRef(ctx context.Context, dir, remote, remoteRef, expectedCommit string) error {
	temporaryRef := fmt.Sprintf("refs/no-mistakes/fetch/%d-%d", os.Getpid(), time.Now().UnixNano())
	defer func() {
		_, _ = Run(context.WithoutCancel(ctx), dir, "update-ref", "--no-deref", "-d", temporaryRef)
	}()
	refspec := fmt.Sprintf("+%s:%s", remoteRef, temporaryRef)
	if _, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, refspec); err != nil {
		return err
	}
	fetched, err := Run(ctx, dir, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("fetched ref %s is not a commit: %w", remoteRef, err)
	}
	if fetched != strings.TrimSpace(expectedCommit) {
		return fmt.Errorf("fetched ref %s moved to %s, expected %s", remoteRef, fetched, expectedCommit)
	}
	return nil
}

// Push pushes HEAD to a remote ref. If forceWithLease is true, it uses an
// explicit expected remote SHA for safe force-push.
func Push(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool) error {
	return PushWithOptions(ctx, dir, remote, ref, expectedSHA, forceWithLease, nil)
}

// PushCommit pushes one immutable commit object to a remote ref. Unlike Push,
// a concurrent worktree HEAD move cannot change the source selected by git.
func PushCommit(ctx context.Context, dir, remote, commitSHA, ref, expectedSHA string, forceWithLease bool) error {
	return pushSourceWithOptions(ctx, dir, remote, commitSHA, ref, expectedSHA, forceWithLease, nil)
}

// PushWithOptions pushes HEAD to a remote with per-push options.
func PushWithOptions(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, "HEAD", ref, expectedSHA, forceWithLease, pushOptions)
}

func pushSourceWithOptions(ctx context.Context, dir, remote, source, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	args := []string{"push"}
	for _, option := range pushOptions {
		args = append(args, "-o", option)
	}
	args = append(args, remote)
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, source+":"+ref)
	_, err := Run(ctx, dir, args...)
	return err
}

// LsRemote returns the SHA of a ref on a remote. Returns empty string if the ref doesn't exist.
func LsRemote(ctx context.Context, dir, remote, ref string) (string, error) {
	out, err := Run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	// Output format: "<sha>\t<ref>"
	parts := strings.Fields(out)
	if len(parts) < 1 {
		return "", nil
	}
	return parts[0], nil
}

// HasUncommittedChanges reports whether the working tree or index differs from HEAD.
// Returns true if any tracked file is modified, staged, or deleted, or if there are
// untracked files. Equivalent to a non-empty `git status --porcelain`.
func HasUncommittedChanges(ctx context.Context, dir string) (bool, error) {
	out, err := Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// UntrackedFiles returns each untracked path in git's order. Ignored files
// are not included.
func UntrackedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := RunRaw(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) < 3 {
			continue
		}
		// Porcelain format: XY <path>\0 where XY is a 2-char status code + space.
		if entry[:2] == "??" {
			files = append(files, entry[3:])
		}
	}
	return files, nil
}

// CreateBranch creates a new branch with the given name and switches to it.
// Fails if the branch already exists.
func CreateBranch(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "checkout", "-b", name)
	return err
}

// CommitAll stages every change in the working tree and creates a single commit
// with the given message. Fails if there are no changes to commit.
func CommitAll(ctx context.Context, dir, message string) error {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		return err
	}
	if !dirty {
		return fmt.Errorf("no changes to commit")
	}
	_, err = Run(ctx, dir, "commit", "-m", message)
	return err
}

// CopyLocalUserIdentity copies local user.name and user.email from srcDir into
// dstDir. Missing values in srcDir are ignored.
//
// The write into dstDir uses per-worktree scope (`git config --worktree`) when
// the repository has worktree config enabled. dstDir is typically a linked
// worktree of the shared gate bare repo, where an unscoped `git config --local`
// write lands in the bare's shared config and takes <bare>/config.lock. Two
// runs starting concurrently on different branches of the same repo then race
// on that single lock and one fails with "could not lock config file ...
// config: File exists". Writing per-worktree puts each run's identity in its own
// <bare>/worktrees/<id>/config.worktree, so concurrent startups never contend.
// Older Git without `--worktree` support falls back to `--local`.
func CopyLocalUserIdentity(ctx context.Context, srcDir, dstDir string) error {
	for _, key := range []string{"user.name", "user.email"} {
		value, err := Run(ctx, srcDir, "config", "--local", "--get", "--default", "", key)
		if err != nil {
			return err
		}
		if value == "" {
			continue
		}
		if _, err := Run(ctx, dstDir, "config", "--worktree", key, value); err != nil {
			if !isWorktreeConfigWriteUnavailable(err) {
				return err
			}
			// Per-worktree config is not usable here (Git too old for the
			// flag, or the repo has multiple worktrees without
			// extensions.worktreeConfig enabled). Fall back to the shared
			// local config. Such gates also lack per-worktree isolation, so
			// this matches the legacy behavior.
			if _, err := Run(ctx, dstDir, "config", "--local", key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// isWorktreeConfigWriteUnavailable reports whether a `git config --worktree`
// write failed because per-worktree config cannot be used on this repo: either
// the installed Git is too old for the flag (isWorktreeConfigUnsupported), or
// the repo has more than one worktree without extensions.worktreeConfig enabled
// ("--worktree cannot be used with multiple working trees unless the config
// extension worktreeConfig is enabled"). Both mean the caller should fall back
// to the shared --local config.
func isWorktreeConfigWriteUnavailable(err error) bool {
	if isWorktreeConfigUnsupported(err) {
		return true
	}
	return strings.Contains(err.Error(), "worktreeConfig")
}

// WorktreeAdd creates a detached worktree at wtPath checked out to the given SHA.
func WorktreeAdd(ctx context.Context, repoDir, wtPath, sha string) error {
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", wtPath, sha)
	return err
}

// WorktreeRemove removes a worktree at the given path.
func WorktreeRemove(ctx context.Context, repoDir, wtPath string) error {
	_, err := Run(ctx, repoDir, "worktree", "remove", "--force", wtPath)
	return err
}

// ResolveRef returns the commit SHA that ref resolves to via
// `git rev-parse --verify <ref>^{commit}`. Use it to pin an exact commit
// (e.g. the default-branch tip just fetched) before reading a file from it,
// so a shared-ref worktree cannot serve a stale remote-tracking ref. Returns
// an error if the ref does not resolve to a commit.
func ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	return out, nil
}

func ExactRefTarget(ctx context.Context, dir, ref string) (string, bool, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		name, target, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && name == ref {
			return strings.TrimSpace(target), true, nil
		}
	}
	return "", false, nil
}

// RefExists reports whether the given ref resolves to a commit. It uses
// `git rev-parse --verify --quiet` so a missing ref is a clean (nil, false)
// result rather than a loud error.
func RefExists(ctx context.Context, dir, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Env = nonInteractiveEnvForContext(ctx, dir)
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return true, nil
}

// ShowFile returns the content of path as stored at the given ref (e.g.
// "HEAD", "origin/main", or a SHA) via `git show <ref>:<path>`. A failure
// (e.g. the path is absent at the ref) is returned as the underlying git
// error from Run; callers that need to distinguish "absent" from a real
// failure should check RefExists first or inspect the error text.
func ShowFile(ctx context.Context, dir, ref, path string) (string, error) {
	out, err := Run(ctx, dir, "show", fmt.Sprintf("%s:%s", ref, path))
	if err != nil {
		return "", err
	}
	return out, nil
}
