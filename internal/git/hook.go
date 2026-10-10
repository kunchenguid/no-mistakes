package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var runGit = RunBare

const gateConfigStampFile = "no-mistakes-gate-config"
const preservedPreReceiveHook = "pre-receive.no-mistakes-user"

func receiveHookOwner(bareDir string) (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("resolve hook executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", "", fmt.Errorf("resolve hook executable: %w", err)
	}
	gate, err := filepath.Abs(bareDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve hook gate: %w", err)
	}
	gate, err = filepath.EvalSymlinks(gate)
	if err != nil {
		return "", "", fmt.Errorf("resolve hook gate: %w", err)
	}
	if _, err := paths.ForGate(gate); err != nil {
		return "", "", err
	}
	return exe, gate, nil
}

// PreReceiveHookScript returns the fail-closed admission hook that runs before
// Git mutates any managed gate ref. The daemon authenticates the hook process's
// ancestry, so a validation-step descendant cannot bypass CLI guards with a
// direct push.
func PreReceiveHookScript(bareDir string) (string, error) {
	exe, gate, err := receiveHookOwner(bareDir)
	if err != nil {
		return "", err
	}
	return preReceiveHookScript(exe, gate), nil
}

func preReceiveHookScript(command, gate string) string {
	return `#!/bin/sh
# no-mistakes pre-receive hook
# Authorize the pushing process before any managed gate ref changes.
NM_BIN=` + shellSingleQuote(command) + `
GATE_DIR=` + shellSingleQuote(gate) + `
NM_HOME=` + shellSingleQuote(filepath.Dir(filepath.Dir(gate))) + `
export NM_HOME
` + receivingGateResolverScript() + `
if ! resolve_receiving_gate; then
  printf 'no-mistakes: gate push refused before ref mutation:\n%s\n' "$RECEIVE_GATE_ERROR" >&2
  exit 1
fi
out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon admit-push --gate "$GATE_DIR" 2>&1)
status=$?
if [ $status -ne 0 ]; then
  printf 'no-mistakes: gate push refused before ref mutation:\n%s\n' "$out" >&2
  exit $status
fi
USER_HOOK="$GATE_DIR/hooks/` + preservedPreReceiveHook + `"
if [ -x "$USER_HOOK" ]; then
  exec "$USER_HOOK"
fi
exit 0
`
}

// receivingGateResolverScript resolves the repository that invoked the hook
// from Git's receive-hook environment and compares it with the canonical gate
// selected at enrollment.
func receivingGateResolverScript() string {
	return `RECEIVE_GATE=
RECEIVE_GATE_ERROR=
resolve_receiving_gate() {
  if [ -z "${GIT_DIR:-}" ]; then
    RECEIVE_GATE_ERROR='cannot resolve receiving Git gate: GIT_DIR is unavailable'
    return 1
  fi
  # Use the fixed utility path: shell pwd can cache an inherited relative PWD.
  RECEIVE_GATE=$(CDPATH= cd -P "$GIT_DIR" 2>/dev/null && /bin/pwd -P) || {
    RECEIVE_GATE_ERROR='cannot resolve receiving Git gate path'
    return 1
  }
  # Resolve both paths in this shell: MSYS can map the enrolled drive path
  # to /tmp (or another mount), rather than the assumed /<drive>/ spelling.
  GATE_COMPARE_DIR=$(CDPATH= cd -P "$GATE_DIR" 2>/dev/null && /bin/pwd -P) || {
    RECEIVE_GATE_ERROR='cannot resolve enrolled Git gate path'
    return 1
  }
  if [ "$RECEIVE_GATE" != "$GATE_COMPARE_DIR" ]; then
    RECEIVE_GATE_ERROR="receiving gate $RECEIVE_GATE does not match enrolled gate $GATE_COMPARE_DIR"
    return 1
  fi
  return 0
}
`
}

func isManagedPreReceiveHook(content []byte) bool {
	text := string(content)
	return strings.Contains(text, "# no-mistakes pre-receive hook") && strings.Contains(text, "daemon admit-push")
}

// RefreshManagedPreReceiveHook installs or refreshes admission while preserving
// an existing user hook behind the managed wrapper.
func RefreshManagedPreReceiveHook(bareDir string) (bool, error) {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	hookPath := filepath.Join(hooksDir, "pre-receive")
	companion := filepath.Join(hooksDir, preservedPreReceiveHook)
	script, err := PreReceiveHookScript(bareDir)
	if err != nil {
		return false, err
	}
	desired := []byte(script)
	existing, err := os.ReadFile(hookPath)
	if err == nil {
		if string(existing) == string(desired) {
			return false, nil
		}
		if !isManagedPreReceiveHook(existing) {
			if _, companionErr := os.Stat(companion); companionErr == nil {
				return false, fmt.Errorf("preserve pre-receive hook: companion already exists")
			} else if !os.IsNotExist(companionErr) {
				return false, companionErr
			}
			if err := os.Rename(hookPath, companion); err != nil {
				return false, fmt.Errorf("preserve pre-receive hook: %w", err)
			}
			if err := writeGateFileAtomic(hookPath, desired, 0o755, ".pre-receive-*"); err != nil {
				_ = os.Rename(companion, hookPath)
				return false, err
			}
			return true, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := writeGateFileAtomic(hookPath, desired, 0o755, ".pre-receive-*"); err != nil {
		return false, err
	}
	return true, nil
}

// RefreshManagedGateHooks owns the complete receive boundary.
func RefreshManagedGateHooks(bareDir string) error {
	if _, err := RefreshManagedPreReceiveHook(bareDir); err != nil {
		return err
	}
	if _, err := RefreshManagedPostReceiveHook(bareDir); err != nil {
		return err
	}
	return nil
}

// PostReceiveHookScript returns the shell script for the post-receive hook.
// The hook notifies the daemon via the enrolled CLI. Enrollment pins the
// canonical gate and runtime paths into the script.
// It never blocks the push - notification failures are surfaced to stderr and
// appended to notify-push.log inside the bare repo.
func PostReceiveHookScript(bareDir string) (string, error) {
	exe, gate, err := receiveHookOwner(bareDir)
	if err != nil {
		return "", err
	}
	return postReceiveHookScript(exe, gate), nil
}

func postReceiveHookScript(command, gate string) string {
	return `#!/bin/sh
# no-mistakes post-receive hook
# Notifies the daemon of the push. Non-blocking: post-receive exit code is
# ignored by git, so we never reject the push here. Instead, failures are
# surfaced on stderr (so the pushing client sees them) and appended to
# notify-push.log inside the bare repo for later inspection.
NM_BIN=` + shellSingleQuote(command) + `
GATE_DIR=` + shellSingleQuote(gate) + `
NM_HOME=` + shellSingleQuote(filepath.Dir(filepath.Dir(gate))) + `
export NM_HOME
` + receivingGateResolverScript() + `
if ! resolve_receiving_gate; then
  LOG="${RECEIVE_GATE:-$GATE_DIR}/notify-push.log"
  {
    printf '[%s] notify-push failed before notification: %s\n\n' "$(date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown)" "$RECEIVE_GATE_ERROR"
  } >> "$LOG" 2>/dev/null || :
  printf 'no-mistakes: notify-push failed before notification: %s\n' "$RECEIVE_GATE_ERROR" >&2
  exit 0
fi
LOG="$RECEIVE_GATE/notify-push.log"
nm_ts() { date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown; }
notify_failed=0
while read oldrev newrev refname; do
	  set -- --gate "$GATE_DIR" \
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
	script, err := PostReceiveHookScript(bareDir)
	if err != nil {
		return err
	}
	return writeHookFileAtomic(hookPath, []byte(script))
}

// RefreshManagedPostReceiveHook updates an existing no-mistakes-owned hook.
// Custom hooks are left untouched; missing hooks are installed for gate repos.
func RefreshManagedPostReceiveHook(bareDir string) (bool, error) {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	script, err := PostReceiveHookScript(bareDir)
	if err != nil {
		return false, err
	}
	desired := []byte(script)
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
	return writeGateFileAtomic(path, content, 0o755, ".post-receive-*")
}

func writeGateFileAtomic(path string, content []byte, mode os.FileMode, pattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// GateConfigCurrent is a subprocess-free restart check for a gate that has
// completed the current hook and config migration. The stamp includes the
// rendered managed hook and a version marker for the non-hook config contract.
// Bump the marker when receive or worktree config requirements change.
func GateConfigCurrent(bareDir string) bool {
	stamp, err := gateConfigStampContent(bareDir)
	if err != nil {
		return false
	}
	content, err := os.ReadFile(filepath.Join(bareDir, gateConfigStampFile))
	if err != nil || string(content) != stamp {
		return false
	}
	// Admission is a security boundary, not merely notification. Verify the
	// managed pre-receive bytes on every startup so a stale stamp cannot hide a
	// removed or replaced guard. This remains filesystem-only for current gates.
	preReceive, err := os.ReadFile(filepath.Join(bareDir, "hooks", "pre-receive"))
	if err != nil {
		return false
	}
	script, err := PreReceiveHookScript(bareDir)
	return err == nil && string(preReceive) == script
}

// MarkGateConfigCurrent atomically records a fully completed gate migration.
// Callers must validate the gate and finish every mutation before marking it.
func MarkGateConfigCurrent(bareDir string) error {
	stamp, err := gateConfigStampContent(bareDir)
	if err != nil {
		return err
	}
	return writeGateFileAtomic(
		filepath.Join(bareDir, gateConfigStampFile),
		[]byte(stamp),
		0o644,
		".no-mistakes-gate-config-*",
	)
}

func gateConfigStampContent(bareDir string) (string, error) {
	exe, gate, err := receiveHookOwner(bareDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("gate-config-v3\x00" + preReceiveHookScript(exe, gate) + "\x00" + postReceiveHookScript(exe, gate)))
	return fmt.Sprintf("v3:%x\n", sum), nil
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
	_, err := EnsureHooksPathIsolation(ctx, bareDir)
	return err
}

func EnsureHooksPathIsolation(ctx context.Context, bareDir string) (bool, error) {
	if _, err := runGit(ctx, bareDir, "config", "--worktree", "--get", "core.hookspath"); err != nil {
		if isWorktreeConfigUnsupported(err) {
			return false, nil
		}
	}
	if _, err := runGit(ctx, bareDir, "config", "extensions.worktreeConfig", "true"); err != nil {
		return false, fmt.Errorf("enable worktree config: %w", err)
	}
	hooksDir, err := filepath.Abs(filepath.Join(bareDir, "hooks"))
	if err != nil {
		return false, fmt.Errorf("resolve hooks dir: %w", err)
	}
	if _, err := runGit(ctx, bareDir, "config", "--worktree", "core.hookspath", hooksDir); err != nil {
		if isWorktreeConfigUnsupported(err) {
			return false, nil
		}
		return false, fmt.Errorf("pin core.hookspath per-worktree: %w", err)
	}
	if err := relocateCoreBareToWorktreeScope(ctx, bareDir); err != nil {
		return false, err
	}
	return true, nil
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
