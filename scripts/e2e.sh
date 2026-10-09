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

# Explicit caller arguments keep the historical single-command override path.
if [[ "$#" -gt 0 ]]; then
  go test "$@"
  code=$?
  exit "$code"
fi

# The canonical internal/e2e package has 104 top-level tests and no parallel
# top-level tests. Three aggregate receipts reached different late tests at the
# 720s, 720s, and 1200s package ceilings without an assertion failure;
# TestUserJourney alone accounted for about five minutes in the first receipt.
# Discovering the top-level names keeps new tests in the canonical gate without
# maintaining a mirrored allowlist. The long native-backend matrix gets its own
# process, and every other test is assigned in discovery order across three
# deterministic shards. Each process owns a meaningful 10-minute timeout while
# this wrapper retains one inventory owner and one EXIT cleanup boundary.
listed_tests="$(go test -tags=e2e -count=1 -timeout 60s -list '^Test' ./internal/e2e)"
code=$?
if [[ "$code" -ne 0 ]]; then
  exit "$code"
fi

tests=()
while IFS= read -r test_name; do
  case "$test_name" in
    Test*)
      if [[ ! "$test_name" =~ ^Test[A-Za-z0-9_]+$ ]]; then
        printf 'e2e: refusing unsafe discovered test name %q\n' "$test_name" >&2
        exit 1
      fi
      tests+=("$test_name")
      ;;
  esac
done <<<"$listed_tests"

if [[ "${#tests[@]}" -eq 0 ]]; then
  printf 'e2e: no top-level tests discovered in ./internal/e2e\n' >&2
  exit 1
fi

shards=("" "" "")
shard_counts=(0 0 0)
journey_found=0
sharded_count=0
for test_name in "${tests[@]}"; do
  if [[ "$test_name" == "TestUserJourney" ]]; then
    journey_found=$((journey_found + 1))
    continue
  fi
  shard=$((sharded_count % 3))
  if [[ -n "${shards[$shard]}" ]]; then
    shards[$shard]="${shards[$shard]}|$test_name"
  else
    shards[$shard]="$test_name"
  fi
  shard_counts[$shard]=$((shard_counts[$shard] + 1))
  sharded_count=$((sharded_count + 1))
done

if [[ "$journey_found" -ne 1 || "$sharded_count" -ne "$((${#tests[@]} - 1))" ]]; then
  printf 'e2e: TestUserJourney discovery was missing or ambiguous\n' >&2
  exit 1
fi

printf 'e2e: running native-backend journey shard (1 test)\n'
go test -tags=e2e -count=1 -timeout 600s -run '^TestUserJourney$' ./internal/e2e
code=$?
if [[ "$code" -ne 0 ]]; then
  exit "$code"
fi

for shard in 0 1 2; do
  if [[ -z "${shards[$shard]}" ]]; then
    continue
  fi
  printf 'e2e: running internal/e2e shard %d/3 (%d tests)\n' "$((shard + 1))" "${shard_counts[$shard]}"
  go test -tags=e2e -count=1 -timeout 600s -run "^(${shards[$shard]})$" ./internal/e2e
  code=$?
  if [[ "$code" -ne 0 ]]; then
    exit "$code"
  fi
done

printf 'e2e: running step-local e2e packages\n'
go test -tags=e2e -count=1 -timeout 600s ./internal/pipeline/steps/...
code=$?
exit "$code"
