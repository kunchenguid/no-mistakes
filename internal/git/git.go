package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
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
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = NonInteractiveEnv(dir)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(stderr))
	}
	return strings.TrimSpace(string(out)), nil
}

// InitBare creates a new bare git repository at the given path.
func InitBare(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", path)
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

// FindGitRoot walks up from path to find the git repository root.
// Resolves symlinks for consistency on macOS (e.g. /tmp -> /private/tmp).
func FindGitRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = abs
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
// repository. For a regular repo this is the same as FindGitRoot. For a
// worktree it resolves back to the main repository root by inspecting
// git's common dir.
func FindMainRepoRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = abs
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	commonDir := strings.TrimSpace(string(out))
	// Make absolute if relative (e.g. ".git" in the main repo itself).
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}
	// commonDir is the .git directory (e.g. /path/to/repo/.git); parent is the repo root.
	root := filepath.Dir(filepath.Clean(commonDir))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root, nil
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

// Push pushes a ref to a remote. If forceWithLease is true, uses
// --force-with-lease with the expectedSHA for safe force-push.
func Push(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool) error {
	return PushWithOptions(ctx, dir, remote, ref, expectedSHA, forceWithLease, nil)
}

// PushWithOptions pushes a ref to a remote with per-push options.
func PushWithOptions(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
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
	args = append(args, "HEAD:"+ref)
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

// RefExists reports whether the given ref resolves to a commit. It uses
// `git rev-parse --verify --quiet` so a missing ref is a clean (nil, false)
// result rather than a loud error.
func RefExists(ctx context.Context, dir, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Env = NonInteractiveEnv(dir)
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
