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
)

var runGit = RunBare

const gateConfigStampFile = "no-mistakes-gate-config"
const preservedPreReceiveHook = "pre-receive.no-mistakes-user"
const preservedReferenceTransactionHook = "reference-transaction.no-mistakes-user"
const receivePackWrapperName = "no-mistakes-receive-pack"

func ReceivePackWrapperPath(bareDir string) string {
	abs, err := filepath.Abs(filepath.Join(bareDir, "hooks", receivePackWrapperName))
	if err != nil {
		return filepath.Join(bareDir, "hooks", receivePackWrapperName)
	}
	return abs
}

func ReceivePackWrapperScript() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "no-mistakes"
	}
	return receivePackWrapperScript(exe)
}

func receivePackWrapperScript(command string) string {
	return `#!/bin/sh
# no-mistakes managed receive-pack wrapper
set -eu
if [ "$#" -lt 1 ]; then
  printf 'no-mistakes: receive-pack repository is missing\n' >&2
  exit 1
fi
GATE_DIR=$1
case "$GATE_DIR" in
  /*) ;;
  *) GATE_DIR=$(cd "$GATE_DIR" 2>/dev/null && (/bin/pwd -P 2>/dev/null || pwd -P) || exit 1) ;;
esac
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
shift
exec "$NM_BIN" daemon receive-pack --gate "$GATE_DIR" "$GATE_DIR" "$@"
`
}

// PreReceiveHookScript returns the fail-closed admission hook that runs before
// Git mutates any managed gate ref.
func PreReceiveHookScript() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "no-mistakes"
	}
	return preReceiveHookScript(exe)
}

func preReceiveHookScript(command string) string {
	return `#!/bin/sh
# no-mistakes pre-receive hook
# Reserve each managed gate ref update before Git mutates it.
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
GATE_DIR=$(git rev-parse --absolute-git-dir 2>/dev/null || :)
case "$GATE_DIR" in
  /*) ;;
  *)
    HOOK_PATH=$0
    case "$HOOK_PATH" in
      */*) HOOK_DIR=${HOOK_PATH%/*} ;;
      *) HOOK_DIR=. ;;
    esac
    GATE_DIR=$(cd "$HOOK_DIR/.." 2>/dev/null && (/bin/pwd -P 2>/dev/null || pwd -P) || :)
    ;;
esac
RECEIVE_INPUT=$(mktemp "$GATE_DIR/.no-mistakes-receive.XXXXXX") || {
  printf 'no-mistakes: cannot reserve gate receive before ref mutation\n' >&2
  exit 1
}
trap 'rm -f "$RECEIVE_INPUT"' 0 1 2 3 15
RECEIVE_SESSION_ID=${NO_MISTAKES_RECEIVE_SESSION_ID:-}
RECEIVE_MANIFEST=${NO_MISTAKES_RECEIVE_MANIFEST:-}
if [ -z "$RECEIVE_SESSION_ID" ] || [ -z "$RECEIVE_MANIFEST" ]; then
  printf 'no-mistakes: receive session is missing\n' >&2
  exit 1
fi
if [ ! -f "$RECEIVE_MANIFEST" ]; then
  printf 'no-mistakes: receive manifest is missing\n' >&2
  exit 1
fi
if ! cat > "$RECEIVE_INPUT"; then
  printf 'no-mistakes: cannot read gate receive updates\n' >&2
  exit 1
fi
ABORT_CAPABILITY_FLAG=--receive-capability-fd
ABORT_CAPABILITY_VALUE=6
if [ -n "${NO_MISTAKES_RECEIVE_CAPABILITY_ABORTED_HANDLE:-}" ]; then
  ABORT_CAPABILITY_FLAG=--receive-capability-handle
  ABORT_CAPABILITY_VALUE=$NO_MISTAKES_RECEIVE_CAPABILITY_ABORTED_HANDLE
fi
abort_receive_batch() {
  if [ "$ABORT_CAPABILITY_FLAG" = --receive-capability-fd ]; then
    abort_out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon receive-transaction --gate "$GATE_DIR" --phase aborted --receive-session-id "$RECEIVE_SESSION_ID" --receive-capability-fd "$ABORT_CAPABILITY_VALUE" < /dev/null 2>&1)
  else
    abort_out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon receive-transaction --gate "$GATE_DIR" --phase aborted --receive-session-id "$RECEIVE_SESSION_ID" "$ABORT_CAPABILITY_FLAG" "$ABORT_CAPABILITY_VALUE" < /dev/null 2>&1)
  fi
  abort_status=$?
  if [ $abort_status -ne 0 ]; then
    printf 'no-mistakes: could not record pre-receive rejection; receive remains fenced:\n%s\n' "$abort_out" >&2
    return $abort_status
  fi
  return 0
}
set --
i=0
while [ "$i" -lt "${GIT_PUSH_OPTION_COUNT:-0}" ]; do
  opt=$(printenv "GIT_PUSH_OPTION_$i" 2>/dev/null || :)
  set -- "$@" --push-option "$opt"
  i=$((i + 1))
done
CAPABILITY_FLAG=--receive-capability-fd
CAPABILITY_VALUE=3
if [ -n "${NO_MISTAKES_RECEIVE_CAPABILITY_ADMIT_HANDLE:-}" ]; then
  CAPABILITY_FLAG=--receive-capability-handle
  CAPABILITY_VALUE=$NO_MISTAKES_RECEIVE_CAPABILITY_ADMIT_HANDLE
fi
if [ "$CAPABILITY_FLAG" = --receive-capability-fd ]; then
  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon admit-push --gate "$GATE_DIR" --receive-session-id "$RECEIVE_SESSION_ID" --receive-capability-fd 3 "$@" < "$RECEIVE_INPUT" 2>&1)
else
  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon admit-push --gate "$GATE_DIR" --receive-session-id "$RECEIVE_SESSION_ID" "$CAPABILITY_FLAG" "$CAPABILITY_VALUE" "$@" < "$RECEIVE_INPUT" 2>&1)
fi
status=$?
if [ $status -ne 0 ]; then
  abort_receive_batch || :
  printf 'no-mistakes: gate push refused before ref mutation:\n%s\n' "$out" >&2
  exit $status
fi
while IFS=' ' read -r reservation_id oldrev newrev refname; do
  [ -z "$refname" ] && continue
  if [ -z "$reservation_id" ] || [ -z "$oldrev" ] || [ -z "$newrev" ] || [ -z "$refname" ]; then
    abort_receive_batch || :
    printf 'no-mistakes: admit push returned an invalid receive identity\n' >&2
    exit 1
  fi
  case "$reservation_id" in
    *[![:alnum:]_-]*)
      abort_receive_batch || :
      printf 'no-mistakes: admit push returned an invalid receive identity\n' >&2
      exit 1
      ;;
  esac
  if ! printf '%s %s %s %s\n' "$reservation_id" "$oldrev" "$newrev" "$refname" >> "$RECEIVE_MANIFEST"; then
    abort_receive_batch || :
    printf 'no-mistakes: cannot persist gate receive identity\n' >&2
    exit 1
  fi
done <<EOF
$out
EOF
USER_HOOK="$GATE_DIR/hooks/` + preservedPreReceiveHook + `"
if [ -x "$USER_HOOK" ]; then
  user_status=0
  "$USER_HOOK" < "$RECEIVE_INPUT" || user_status=$?
  if [ $user_status -ne 0 ]; then
    abort_receive_batch || :
    exec 3<&- 4<&- 5<&- 6<&- 7<&- 8<&-
    exit $user_status
  fi
  exec 3<&- 4<&- 5<&- 6<&- 7<&- 8<&-
fi
exit 0
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
	desired := []byte(PreReceiveHookScript())
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
	if _, err := RefreshManagedReceivePackWrapper(bareDir); err != nil {
		return err
	}
	if _, err := RefreshManagedPreReceiveHook(bareDir); err != nil {
		return err
	}
	if _, err := RefreshManagedReferenceTransactionHook(bareDir); err != nil {
		return err
	}
	if _, err := RefreshManagedPostReceiveHook(bareDir); err != nil {
		return err
	}
	return nil
}

func RefreshManagedReceivePackWrapper(bareDir string) (bool, error) {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	path := ReceivePackWrapperPath(bareDir)
	desired := []byte(ReceivePackWrapperScript())
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == string(desired) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := writeGateFileAtomic(path, desired, 0o755, ".receive-pack-*"); err != nil {
		return false, err
	}
	return true, nil
}

func ReferenceTransactionHookScript() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "no-mistakes"
	}
	return referenceTransactionHookScript(exe)
}

func referenceTransactionHookScript(command string) string {
	return `#!/bin/sh
# no-mistakes reference-transaction hook
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
PHASE=$1
case "$PHASE" in
  prepared|committed|aborted) ;;
  *)
    printf 'no-mistakes: unsupported reference transaction phase %s\n' "$PHASE" >&2
    exit 1
    ;;
esac
GATE_DIR=$(git rev-parse --absolute-git-dir 2>/dev/null || :)
case "$GATE_DIR" in
  /*) ;;
  *)
    HOOK_PATH=$0
    case "$HOOK_PATH" in
      */*) HOOK_DIR=${HOOK_PATH%/*} ;;
      *) HOOK_DIR=. ;;
    esac
    GATE_DIR=$(cd "$HOOK_DIR/.." 2>/dev/null && (/bin/pwd -P 2>/dev/null || pwd -P) || :)
    ;;
esac
case "$GATE_DIR" in
  /*) ;;
  *) GATE_DIR=$(/bin/pwd -P 2>/dev/null || pwd -P 2>/dev/null || pwd) ;;
esac
USER_HOOK="$GATE_DIR/hooks/` + preservedReferenceTransactionHook + `"
RECEIVE_SESSION_ID=${NO_MISTAKES_RECEIVE_SESSION_ID:-}
RECEIVE_MANIFEST=${NO_MISTAKES_RECEIVE_MANIFEST:-}
INTERNAL_CAPABILITY=${NO_MISTAKES_INTERNAL_MUTATION_CAPABILITY:-}
INTERNAL_OPERATION=${NO_MISTAKES_INTERNAL_MUTATION_OPERATION:-}
INTERNAL_BRANCH=${NO_MISTAKES_INTERNAL_MUTATION_BRANCH:-}
INTERNAL_AUTHORITY=${NO_MISTAKES_INTERNAL_MUTATION_AUTHORITY:-}
if [ -z "$RECEIVE_SESSION_ID" ] && [ -z "$RECEIVE_MANIFEST" ]; then

  RECEIVE_INPUT=$(mktemp "$GATE_DIR/.no-mistakes-reference-transaction.XXXXXX") || {
    printf 'no-mistakes: cannot record unmanaged reference transaction\n' >&2
    exit 1
  }
  trap 'rm -f "$RECEIVE_INPUT"' 0 1 2 3 15
  if ! cat > "$RECEIVE_INPUT"; then
    printf 'no-mistakes: cannot read unmanaged reference transaction\n' >&2
    exit 1
  fi
  MANAGED_COUNT=0
  INTERNAL_OLD=
  INTERNAL_NEW=
  INTERNAL_REF=
  while IFS=' ' read -r oldrev newrev refname; do
    [ -z "$refname" ] && continue
    case "$refname" in
      refs/heads/*|refs/no-mistakes/*)
        MANAGED_COUNT=$((MANAGED_COUNT + 1))
        INTERNAL_OLD=$oldrev
        INTERNAL_NEW=$newrev
        INTERNAL_REF=$refname
        ;;
    esac
  done < "$RECEIVE_INPUT"
  if [ "$MANAGED_COUNT" -eq 0 ]; then
    if [ -x "$USER_HOOK" ]; then
      "$USER_HOOK" "$PHASE" < "$RECEIVE_INPUT"
      exit $?
    fi
    exit 0
  fi
  if [ -z "$INTERNAL_CAPABILITY" ]; then
    printf 'no-mistakes: internal mutation capability is required for managed ref %s\n' "$INTERNAL_REF" >&2
    exit 1
  fi
  if [ -z "$INTERNAL_OPERATION" ] || [ -z "$INTERNAL_BRANCH" ] || [ -z "$INTERNAL_AUTHORITY" ]; then
    printf 'no-mistakes: internal mutation capability metadata is incomplete\n' >&2
    exit 1
  fi
  if [ "$MANAGED_COUNT" -ne 1 ]; then
    printf 'no-mistakes: one exact internal mutation capability is required per managed ref transaction\n' >&2
    exit 1
  fi
  case "$INTERNAL_REF" in
    refs/heads/*) INTERNAL_SCOPE=ordinary ;;
    refs/no-mistakes/*) INTERNAL_SCOPE=private ;;
    *)
      printf 'no-mistakes: internal mutation capability cannot authorize ref %s\n' "$INTERNAL_REF" >&2
      exit 1
      ;;
  esac
  NM_HOOK_HELPER=1 "$NM_BIN" daemon authorize-ref-mutation --gate "$GATE_DIR" --authority "$INTERNAL_AUTHORITY" --phase "$PHASE" --branch "$INTERNAL_BRANCH" --ref "$INTERNAL_REF" --old "$INTERNAL_OLD" --new "$INTERNAL_NEW" --operation "$INTERNAL_OPERATION" --scope "$INTERNAL_SCOPE" < /dev/null
  status=$?
  if [ $status -ne 0 ]; then
    printf 'no-mistakes: internal mutation capability refused for %s\n' "$INTERNAL_REF" >&2
    exit $status
  fi
  if [ -x "$USER_HOOK" ]; then
    "$USER_HOOK" "$PHASE" < "$RECEIVE_INPUT"
    exit $?
  fi
  exit 0
fi
if [ -z "$RECEIVE_SESSION_ID" ] || [ -z "$RECEIVE_MANIFEST" ] || [ ! -f "$RECEIVE_MANIFEST" ]; then
  printf 'no-mistakes: receive session evidence is incomplete\n' >&2
  exit 1
fi
RECEIVE_INPUT=$(mktemp "$GATE_DIR/.no-mistakes-reference-transaction.XXXXXX") || {
  printf 'no-mistakes: cannot record reference transaction evidence\n' >&2
  exit 1
}
trap 'rm -f "$RECEIVE_INPUT"' 0 1 2 3 15
if ! cat > "$RECEIVE_INPUT"; then
  printf 'no-mistakes: cannot read reference transaction updates\n' >&2
  exit 1
fi
TRANSACTION_INPUT=$(mktemp "$GATE_DIR/.no-mistakes-reference-evidence.XXXXXX") || {
  printf 'no-mistakes: cannot prepare reference transaction evidence\n' >&2
  exit 1
}
trap 'rm -f "$RECEIVE_INPUT" "$TRANSACTION_INPUT"' 0 1 2 3 15
while IFS=' ' read -r oldrev newrev refname; do
  [ -z "$refname" ] && continue
  reservation_id=
  matches=0
  while IFS=' ' read -r candidate candidate_old candidate_new candidate_ref; do
    if [ "$candidate_old" = "$oldrev" ] && [ "$candidate_new" = "$newrev" ] && [ "$candidate_ref" = "$refname" ]; then
      reservation_id=$candidate
      matches=$((matches + 1))
    fi
  done < "$RECEIVE_MANIFEST"
  if [ "$matches" -ne 1 ]; then
    printf 'no-mistakes: receive session evidence does not match %s\n' "$refname" >&2
    exit 1
  fi
  if ! printf '%s %s %s %s\n' "$reservation_id" "$oldrev" "$newrev" "$refname" >> "$TRANSACTION_INPUT"; then
    printf 'no-mistakes: cannot persist reference transaction evidence\n' >&2
    exit 1
  fi
done < "$RECEIVE_INPUT"
CAPABILITY_FLAG=--receive-capability-fd
CAPABILITY_FD=4
case "$PHASE" in
  committed) CAPABILITY_FD=5 ;;
  aborted) CAPABILITY_FD=6 ;;
esac
case "$PHASE" in
  prepared) CAPABILITY_HANDLE=${NO_MISTAKES_RECEIVE_CAPABILITY_PREPARED_HANDLE:-} ;;
  committed) CAPABILITY_HANDLE=${NO_MISTAKES_RECEIVE_CAPABILITY_COMMITTED_HANDLE:-} ;;
  aborted) CAPABILITY_HANDLE=${NO_MISTAKES_RECEIVE_CAPABILITY_ABORTED_HANDLE:-} ;;
  *) CAPABILITY_HANDLE= ;;
esac
if [ -n "$CAPABILITY_HANDLE" ]; then
  CAPABILITY_FLAG=--receive-capability-handle
fi
if [ "$CAPABILITY_FLAG" = --receive-capability-fd ]; then
  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon receive-transaction --gate "$GATE_DIR" --phase "$PHASE" --receive-session-id "$RECEIVE_SESSION_ID" --receive-capability-fd "$CAPABILITY_FD" < "$TRANSACTION_INPUT" 2>&1)
else
  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon receive-transaction --gate "$GATE_DIR" --phase "$PHASE" --receive-session-id "$RECEIVE_SESSION_ID" "$CAPABILITY_FLAG" "$CAPABILITY_HANDLE" < "$TRANSACTION_INPUT" 2>&1)
fi
status=$?
if [ $status -ne 0 ]; then
  printf 'no-mistakes: reference transaction evidence refused (%s):\n%s\n' "$PHASE" "$out" >&2
  exit $status
fi
if [ -x "$USER_HOOK" ]; then
  exec 3<&- 4>&- 5<&- 6<&- 7<&- 8<&-
  "$USER_HOOK" "$PHASE" < "$RECEIVE_INPUT"
  exit $?
fi
exit 0
`
}

func isManagedReferenceTransactionHook(content []byte) bool {
	text := string(content)
	return strings.Contains(text, "# no-mistakes reference-transaction hook") && strings.Contains(text, "daemon receive-transaction")
}

func RefreshManagedReferenceTransactionHook(bareDir string) (bool, error) {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	hookPath := filepath.Join(hooksDir, "reference-transaction")
	companion := filepath.Join(hooksDir, preservedReferenceTransactionHook)
	desired := []byte(ReferenceTransactionHookScript())
	existing, err := os.ReadFile(hookPath)
	if err == nil {
		if string(existing) == string(desired) {
			return false, nil
		}
		if !isManagedReferenceTransactionHook(existing) {
			if _, companionErr := os.Stat(companion); companionErr == nil {
				return false, fmt.Errorf("preserve reference-transaction hook: companion already exists")
			} else if !os.IsNotExist(companionErr) {
				return false, companionErr
			}
			if err := os.Rename(hookPath, companion); err != nil {
				return false, fmt.Errorf("preserve reference-transaction hook: %w", err)
			}
			if err := writeGateFileAtomic(hookPath, desired, 0o755, ".reference-transaction-*"); err != nil {
				_ = os.Rename(companion, hookPath)
				return false, err
			}
			return true, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := writeGateFileAtomic(hookPath, desired, 0o755, ".reference-transaction-*"); err != nil {
		return false, err
	}
	return true, nil
}

// PostReceiveHookScript returns the shell script for the post-receive hook.
// The hook notifies the daemon via the CLI so it works across platforms.
// It resolves the gate to an absolute bare-repo path before notifying.
// It never rejects the completed ref update - notification failures are
// retried, surfaced to stderr, and appended to notify-push.log inside the
// bare repo.
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
# Notifies the daemon of the push. A bounded retry handles a short ownership
# transition; remaining failures are surfaced on stderr and appended to
# notify-push.log inside the bare repo for later startup reconciliation.
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
# Resolve the bare repo dir explicitly. Git can invoke this hook from a cwd
# whose pwd collapses to "." (issue #269), which would pass "--gate ." and be
# rejected by the daemon ("invalid gate path: ."), so the pipeline never
# starts. Prefer git's own absolute dir query (Git 2.13+, May 2017), then fall
# back to the hook file's location so a poisoned PWD still cannot produce ".".
GATE_DIR=$(git rev-parse --absolute-git-dir 2>/dev/null || :)
case "$GATE_DIR" in
  /*) ;;
  *)
    HOOK_PATH=$0
    case "$HOOK_PATH" in
      */*) HOOK_DIR=${HOOK_PATH%/*} ;;
      *) HOOK_DIR=. ;;
    esac
    GATE_DIR=$(cd "$HOOK_DIR/.." 2>/dev/null && (/bin/pwd -P 2>/dev/null || pwd -P) || :)
    ;;
esac
case "$GATE_DIR" in
  /*) ;;
  *) GATE_DIR=$(/bin/pwd -P 2>/dev/null || pwd -P 2>/dev/null || pwd) ;;
esac
LOG="$GATE_DIR/notify-push.log"
RECEIVE_SESSION_ID=${NO_MISTAKES_RECEIVE_SESSION_ID:-}
RECEIVE_MANIFEST=${NO_MISTAKES_RECEIVE_MANIFEST:-}
if [ -z "$RECEIVE_SESSION_ID" ] || [ -z "$RECEIVE_MANIFEST" ] || [ ! -f "$RECEIVE_MANIFEST" ]; then
  printf 'no-mistakes: receive session is missing; refusing unbound notification\n' >&2
  exit 1
fi
nm_ts() { date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown; }
RECEIVE_INPUT=$(mktemp "$GATE_DIR/.no-mistakes-post-receive.XXXXXX") || exit 1
trap 'rm -f "$RECEIVE_INPUT"' 0 1 2 3 15
if ! cat > "$RECEIVE_INPUT"; then
  printf 'no-mistakes: cannot read post-receive updates\n' >&2
  exit 1
fi
notify_failed=0
CAPABILITY_FLAG=--receive-capability-fd
CAPABILITY_VALUE=7
if [ -n "${NO_MISTAKES_RECEIVE_CAPABILITY_NOTIFY_HANDLE:-}" ]; then
  CAPABILITY_FLAG=--receive-capability-handle
  CAPABILITY_VALUE=$NO_MISTAKES_RECEIVE_CAPABILITY_NOTIFY_HANDLE
fi
set -- --gate "$GATE_DIR" --receive-session-id "$RECEIVE_SESSION_ID" "$CAPABILITY_FLAG" "$CAPABILITY_VALUE"
i=0
while [ "$i" -lt "${GIT_PUSH_OPTION_COUNT:-0}" ]; do
  opt=$(printenv "GIT_PUSH_OPTION_$i" 2>/dev/null || :)
  set -- "$@" --push-option "$opt"
  i=$((i + 1))
done
attempt=1
out=""
status=1
while [ "$attempt" -le 3 ]; do
  out=$(NM_HOOK_HELPER=1 "$NM_BIN" daemon notify-push "$@" < "$RECEIVE_INPUT" 2>&1)
  status=$?
  [ $status -eq 0 ] && break
  if [ "$attempt" -lt 3 ]; then
    sleep 1
  fi
  attempt=$((attempt + 1))
done
  if [ $status -ne 0 ]; then
    notify_failed=1
    {
      printf '[%s] notify-push failed for batch (exit %d)\n' "$(nm_ts)" "$status"
      printf '%s\n\n' "$out"
    } >> "$LOG"
    {
      printf 'no-mistakes: notify-push failed for batch (exit %d):\n' "$status"
      printf '%s\n' "$out"
      printf 'See %s for full history.\n' "$LOG"
    } >&2
  fi

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
	if !isBareGitDir(bareDir) {
		return false
	}
	content, err := os.ReadFile(filepath.Join(bareDir, gateConfigStampFile))
	if err != nil || string(content) != gateConfigStampContent() {
		return false
	}
	// Admission is a security boundary, not merely notification. Verify the
	// managed pre-receive bytes on every startup so a stale stamp cannot hide a
	// removed or replaced guard. This remains filesystem-only for current gates.
	preReceive, err := os.ReadFile(filepath.Join(bareDir, "hooks", "pre-receive"))
	if err != nil || string(preReceive) != PreReceiveHookScript() {
		return false
	}
	if !gateHookExecutable(filepath.Join(bareDir, "hooks", "pre-receive")) {
		return false
	}
	referenceTransaction, err := os.ReadFile(filepath.Join(bareDir, "hooks", "reference-transaction"))
	if err != nil || string(referenceTransaction) != ReferenceTransactionHookScript() {
		return false
	}
	if !gateHookExecutable(filepath.Join(bareDir, "hooks", "reference-transaction")) {
		return false
	}
	receivePack, err := os.ReadFile(ReceivePackWrapperPath(bareDir))
	if err != nil || string(receivePack) != ReceivePackWrapperScript() || !gateHookExecutable(ReceivePackWrapperPath(bareDir)) {
		return false
	}
	return gateWorktreeConfigCurrent(bareDir)
}

func gateHookExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func gateWorktreeConfigCurrent(bareDir string) bool {
	hooksDir, err := filepath.Abs(filepath.Join(bareDir, "hooks"))
	if err != nil {
		return false
	}
	config, err := os.ReadFile(filepath.Join(bareDir, "config.worktree"))
	if err != nil {
		return false
	}
	values := map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(config), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "core" {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		values[key] = value
	}
	configuredHooks, ok := values["hookspath"]
	if !ok || filepath.IsAbs(configuredHooks) == false || filepath.Clean(configuredHooks) != filepath.Clean(hooksDir) {
		return false
	}
	return strings.EqualFold(values["bare"], "true")
}

// MarkGateConfigCurrent atomically records a fully completed gate migration.
// Callers must validate the gate and finish every mutation before marking it.
func MarkGateConfigCurrent(bareDir string) error {
	return writeGateFileAtomic(
		filepath.Join(bareDir, gateConfigStampFile),
		[]byte(gateConfigStampContent()),
		0o644,
		".no-mistakes-gate-config-*",
	)
}

func gateConfigStampContent() string {
	sum := sha256.Sum256([]byte("gate-config-v4\x00" + ReceivePackWrapperScript() + "\x00" + PreReceiveHookScript() + "\x00" + ReferenceTransactionHookScript() + "\x00" + PostReceiveHookScript()))
	return fmt.Sprintf("v4:%x\n", sum)
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
