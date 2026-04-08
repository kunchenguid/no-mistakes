package git

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Run executes a git command in the given directory and returns trimmed stdout.
// Returns an error that includes the command and stderr on failure.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
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

// RemoveRemote removes a named remote from the repo at dir.
func RemoveRemote(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "remote", "remove", name)
	return err
}

// GetRemoteURL returns the URL of a named remote.
func GetRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "remote", "get-url", name)
}

// FindGitRoot walks up from path to find the git repository root.
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
	return strings.TrimSpace(string(out)), nil
}

// Diff returns the unified diff between two commits.
func Diff(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "diff", base+".."+head)
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

// Push pushes a ref to a remote. If forceWithLease is true, uses
// --force-with-lease with the expectedSHA for safe force-push.
func Push(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool) error {
	args := []string{"push", remote}
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
