package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var runGit = Run

// PostReceiveHookScript returns the shell script for the post-receive hook.
// The hook notifies the daemon via the CLI so it works across platforms.
// It never blocks the push - notification failures are surfaced to stderr and
// appended to notify-push.log inside the bare repo.
func PostReceiveHookScript() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "no-mistakes"
	}
	return postReceiveHookScript(exe)
}

func postReceiveHookScript(command string) string {
	return `#!/bin/sh
# no-mistakes post-receive hook
# Notifies the daemon of the push. Non-blocking: post-receive exit code is
# ignored by git, so we never reject the push here. Instead, failures are
# surfaced on stderr (so the pushing client sees them) and appended to
# notify-push.log inside the bare repo for later inspection.
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
LOG="$(pwd)/notify-push.log"
nm_ts() { date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown; }
notify_failed=0
while read oldrev newrev refname; do
	  set -- --gate "$(pwd)" \
	    --ref "$refname" \
	    --old "$oldrev" \
	    --new "$newrev"
	  i=0
	  while [ "$i" -lt "${GIT_PUSH_OPTION_COUNT:-0}" ]; do
	    opt=$(printenv "GIT_PUSH_OPTION_$i" 2>/dev/null || :)
	    set -- "$@" --push-option "$opt"
	    i=$((i + 1))
	  done
	  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon notify-push "$@" 2>&1)
  status=$?
  if [ $status -ne 0 ]; then
    notify_failed=1
    {
      printf '[%s] notify-push failed for %s (exit %d)\n' "$(nm_ts)" "$refname" "$status"
      printf '%s\n\n' "$out"
    } >> "$LOG"
    {
      printf 'no-mistakes: notify-push failed for %s (exit %d):\n' "$refname" "$status"
      printf '%s\n' "$out"
      printf 'See %s for full history.\n' "$LOG"
    } >&2
  fi
done

if [ "$notify_failed" -eq 0 ]; then
  cat >&2 <<'BANNER'
_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____
|\ | |  |    |\/| | [__   |  |__| |_/  |___ [__
| \| |__|    |  | | ___]  |  |  | | \_ |___ ___]

  * Pipeline started

  Run no-mistakes to review.

BANNER
fi
exit 0
`
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isManagedPostReceiveHook(content []byte) bool {
	text := string(content)
	return strings.Contains(text, "# no-mistakes post-receive hook") && strings.Contains(text, "daemon notify-push")
}

// InstallPostReceiveHook writes the post-receive hook script into
// the hooks directory of a bare repo at bareDir.
func InstallPostReceiveHook(bareDir string) error {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	return writeHookFileAtomic(hookPath, []byte(PostReceiveHookScript()))
}

// RefreshManagedPostReceiveHook updates an existing no-mistakes-owned hook.
// Custom hooks are left untouched; missing hooks are installed for gate repos.
func RefreshManagedPostReceiveHook(bareDir string) (bool, error) {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	desired := []byte(PostReceiveHookScript())
	existing, err := os.ReadFile(hookPath)
	if err == nil {
		if string(existing) == string(desired) {
			return false, nil
		}
		if !isManagedPostReceiveHook(existing) {
			return false, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := writeHookFileAtomic(hookPath, desired); err != nil {
		return false, err
	}
	return true, nil
}

func writeHookFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".post-receive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// IsolateHooksPath protects the gate's post-receive hook from being
// disabled when a pipeline subprocess (e.g. husky during `pnpm install`)
// runs `git config core.hookspath` from inside a linked worktree.
//
// Linked worktrees share the bare's local config, so an unscoped
// `git config` write lands in <bareDir>/config and silently overrides
// the gate's hooks lookup. To defend against this, we enable
// extensions.worktreeConfig on the bare and pin core.hookspath in the
// bare's per-worktree config (<bareDir>/config.worktree). Per-worktree
// scope wins over local, so the bare's main worktree always resolves
// hooks to its own absolute hooks dir, regardless of what tools write
// to the shared config.
//
// Enabling extensions.worktreeConfig also forces us to relocate
// core.bare: once the extension is on, Git requires core.bare and
// core.worktree to live in per-worktree scope only. If we leave
// core.bare=true in shared config, it leaks into linked worktrees and
// causes commands like `git rebase` to fail with "this operation must
// be run in a work tree". It also prevents provider CLIs such as gh from
// resolving the repo from a CI step worktree cwd.
//
// Best-effort only: if the installed Git does not support
// `git config --worktree`, this returns nil without changing config.
//
// Idempotent: safe to call on an already-configured bare repo to
// migrate older installs when per-worktree config is available.
func IsolateHooksPath(ctx context.Context, bareDir string) error {
	if _, err := runGit(ctx, bareDir, "config", "--worktree", "--get", "core.hookspath"); err != nil {
		if isWorktreeConfigUnsupported(err) {
			return nil
		}
	}
	if _, err := runGit(ctx, bareDir, "config", "extensions.worktreeConfig", "true"); err != nil {
		return fmt.Errorf("enable worktree config: %w", err)
	}
	hooksDir, err := filepath.Abs(filepath.Join(bareDir, "hooks"))
	if err != nil {
		return fmt.Errorf("resolve hooks dir: %w", err)
	}
	if _, err := runGit(ctx, bareDir, "config", "--worktree", "core.hookspath", hooksDir); err != nil {
		if isWorktreeConfigUnsupported(err) {
			return nil
		}
		return fmt.Errorf("pin core.hookspath per-worktree: %w", err)
	}
	return relocateCoreBareToWorktreeScope(ctx, bareDir)
}

// relocateCoreBareToWorktreeScope moves core.bare out of shared local config
// into the bare's per-worktree config. Required after enabling
// extensions.worktreeConfig: Git otherwise leaks core.bare=true from shared
// scope into linked worktrees, breaking rebase/merge/etc. and provider CLI
// repo resolution from worktree cwd.
func relocateCoreBareToWorktreeScope(ctx context.Context, bareDir string) error {
	if _, err := runGit(ctx, bareDir, "config", "--worktree", "core.bare", "true"); err != nil {
		if isWorktreeConfigUnsupported(err) {
			return nil
		}
		return fmt.Errorf("pin core.bare per-worktree: %w", err)
	}
	if _, err := runGit(ctx, bareDir, "config", "--local", "--unset", "core.bare"); err != nil {
		if isConfigKeyMissing(err) {
			return nil
		}
		return fmt.Errorf("unset shared core.bare: %w", err)
	}
	return nil
}

// isConfigKeyMissing reports whether a `git config --unset` failure is the
// benign "key not set" case (exit 5), which makes the unset idempotent.
func isConfigKeyMissing(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 5
}

func isWorktreeConfigUnsupported(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "unknown option") && strings.Contains(msg, "worktree")
}
