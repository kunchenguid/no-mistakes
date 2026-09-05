#!/usr/bin/env bash
# Publish-firewall GitHub check. Stdout/stderr uploaded to GitHub MUST stay
# generic. Diffs, titles, and findings are written only to RUNNER_TEMP (or
# POSTed to the LAN portal) and never echoed.
set -eu

generic_fail() {
  echo "publish-policy violation"
  if [ -n "${NM_PORTAL_URL:-}" ]; then
    echo "${NM_PORTAL_URL}"
  fi
  echo "conclusion=error" >> "${GITHUB_OUTPUT:-/dev/null}"
  exit 1
}

if ! command -v no-mistakes >/dev/null 2>&1; then
  echo "publish-policy violation" >&2
  generic_fail
fi

TMP="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
DIFF="$TMP/nm-firewall.diff"
TITLE="$TMP/nm-firewall.title"
BODY="$TMP/nm-firewall.body"
COMMITS="$TMP/nm-firewall.commits"
PRIVATE="$TMP/nm-firewall.private.json"
: >"$DIFF"
: >"$TITLE"
: >"$BODY"
: >"$COMMITS"

if [ -z "${GITHUB_EVENT_PATH:-}" ] || [ ! -f "${GITHUB_EVENT_PATH}" ]; then
  generic_fail
fi

if command -v python3 >/dev/null 2>&1; then
  PY=python3
elif command -v python >/dev/null 2>&1; then
  PY=python
else
  generic_fail
fi

NM_TITLE_FILE="$TITLE" NM_BODY_FILE="$BODY" "$PY" - <<'PY'
import json, os
event = json.load(open(os.environ["GITHUB_EVENT_PATH"]))
pr = event.get("pull_request") or {}
open(os.environ["NM_TITLE_FILE"], "w", encoding="utf-8").write(pr.get("title") or "")
open(os.environ["NM_BODY_FILE"], "w", encoding="utf-8").write(pr.get("body") or "")
PY

BASE_SHA="${NM_BASE_SHA:-}"
HEAD_SHA="${NM_HEAD_SHA:-}"
if [ -n "$BASE_SHA" ] && [ -n "$HEAD_SHA" ] && command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  git fetch --quiet origin "$BASE_SHA" "$HEAD_SHA" >/dev/null 2>&1 || true
  if git diff --no-color "${BASE_SHA}...${HEAD_SHA}" >"$DIFF" 2>/dev/null; then
    :
  else
    generic_fail
  fi
else
  generic_fail
fi

if [ ! -s "$DIFF" ]; then
  # An empty diff is a successful scan of nothing, but a missing fetch is not.
  # Treat a zero-byte file after a successful git diff as clean input.
  :
fi

GH_BIN=""
if command -v gh-axi >/dev/null 2>&1; then
  GH_BIN=gh-axi
elif command -v gh >/dev/null 2>&1; then
  GH_BIN=gh
fi
if [ -n "$GH_BIN" ] && [ -n "${NM_REPO:-}" ] && [ -n "${NM_PR_NUMBER:-}" ]; then
  "$GH_BIN" api "repos/${NM_REPO}/pulls/${NM_PR_NUMBER}/commits" >"$TMP/nm-firewall.commits.json" 2>/dev/null || true
  if [ -s "$TMP/nm-firewall.commits.json" ]; then
    NM_COMMITS_IN="$TMP/nm-firewall.commits.json" NM_COMMITS_OUT="$COMMITS" "$PY" - <<'PY'
import json, os
raw = open(os.environ["NM_COMMITS_IN"], encoding="utf-8").read()
try:
    data = json.loads(raw)
except json.JSONDecodeError:
    raise SystemExit(0)
msgs = []
if isinstance(data, list):
    for item in data:
        commit = (item or {}).get("commit") or {}
        msgs.append((commit.get("message") or "").split("\n", 1)[0])
open(os.environ["NM_COMMITS_OUT"], "w", encoding="utf-8").write("\n".join(msgs))
PY
  fi
fi

set +e
no-mistakes firewall github-check \
  --diff "$DIFF" \
  --title-file "$TITLE" \
  --body-file "$BODY" \
  --commits-file "$COMMITS" \
  --repo "${NM_REPO:-}" \
  --pr-number "${NM_PR_NUMBER:-0}" \
  --branch "${NM_HEAD_REF:-}" \
  --head "${HEAD_SHA}" \
  --base "${BASE_SHA}" \
  --portal-url "${NM_PORTAL_URL:-}" \
  --portal-token "${NM_PORTAL_TOKEN:-}" \
  --private-json "$PRIVATE"
status=$?
set -e

if [ "$status" -eq 0 ]; then
  echo "conclusion=success" >> "${GITHUB_OUTPUT:-/dev/null}"
  exit 0
fi
echo "conclusion=failure" >> "${GITHUB_OUTPUT:-/dev/null}"
exit 1
