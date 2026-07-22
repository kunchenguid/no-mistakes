#!/usr/bin/env bash
# Suite wrapper for temporary E2E daemon ownership.
#
# Responsibilities:
#   1. Exact inventory directory (mode 0700) for this suite invocation
#   2. EXIT/INT/TERM trap that reaps only inventoried temp daemons
#   3. Concurrency cap via NM_E2E_DAEMON_MAX (default 2)
#   4. Pre-reap of any leftover inventory from a prior killed wrapper
#
# Honest boundary: this EXIT trap does NOT survive SIGKILL of this shell.
# When the wrapper itself is SIGKILL'd, the on-disk inventory is recovered
# on the next suite start (this script's pre-reap + package TestMain).
# Child go-test interruption/timeout/SIGKILL is covered: this shell still
# runs the trap and reaps.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

if [[ -z "${NM_E2E_DAEMON_INVENTORY:-}" ]]; then
  base="/tmp"
  if [[ -d /private/tmp ]]; then
    base="/private/tmp"
  fi
  NM_E2E_DAEMON_INVENTORY_PARENT="${base}/no-mistakes-e2e-inventories-$(id -u)"
  if [[ -L "$NM_E2E_DAEMON_INVENTORY_PARENT" ]]; then
    exit 1
  fi
  mkdir -p "$NM_E2E_DAEMON_INVENTORY_PARENT" || exit 1
  chmod 700 "$NM_E2E_DAEMON_INVENTORY_PARENT" || exit 1
  NM_E2E_DAEMON_INVENTORY="$(mktemp -d "${NM_E2E_DAEMON_INVENTORY_PARENT}/run-XXXXXX")" || exit 1
  export NM_E2E_DAEMON_INVENTORY
  export NM_E2E_DAEMON_INVENTORY_PARENT
  chmod 700 "$NM_E2E_DAEMON_INVENTORY" || exit 1
  printf '%s\n' "$$" >"$NM_E2E_DAEMON_INVENTORY/owner.pid" || exit 1
  chmod 600 "$NM_E2E_DAEMON_INVENTORY/owner.pid" || exit 1
  OWNED_INVENTORY=1
else
  mkdir -p "$NM_E2E_DAEMON_INVENTORY"
  chmod 700 "$NM_E2E_DAEMON_INVENTORY" 2>/dev/null || true
  OWNED_INVENTORY=0
fi

export NM_E2E_DAEMON_MAX="${NM_E2E_DAEMON_MAX:-2}"

reap_inventory() {
  # Best-effort; never expand into shared-daemon territory (reaper refuses).
  (cd "$ROOT" && go run ./internal/e2edaemon/reapmain.go) >/dev/null 2>&1 || true
}

if [[ -n "${NM_E2E_DAEMON_INVENTORY_PARENT:-}" ]]; then
  export NM_E2E_REAP_ABANDONED=1
  reap_inventory
  unset NM_E2E_REAP_ABANDONED
fi

trap 'reap_inventory; if [[ "${OWNED_INVENTORY}" -eq 1 ]]; then rm -rf "$NM_E2E_DAEMON_INVENTORY" 2>/dev/null || true; fi' EXIT INT TERM

# Default args match the historical Makefile e2e target; callers may override.
if [[ "$#" -eq 0 ]]; then
  set -- -tags=e2e -count=1 -timeout 300s ./internal/e2e/... ./internal/pipeline/steps/...
fi

go test "$@"
code=$?
exit "$code"
