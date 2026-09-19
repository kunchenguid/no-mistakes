#!/usr/bin/env bash
# Publish-firewall GitHub check. Stdout/stderr uploaded to GitHub MUST stay
# generic. Diffs, titles, and findings are written only to RUNNER_TEMP (or
# POSTed to the LAN portal) and never echoed.
set -eu

generic_fail() {
  echo "publish-policy error"
  if [ -n "${NM_PORTAL_URL:-}" ]; then
    echo "${NM_PORTAL_URL}"
  fi
  echo "conclusion=error" >> "${GITHUB_OUTPUT:-/dev/null}"
  exit 1
}

if ! command -v no-mistakes >/dev/null 2>&1; then
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

if command -v python3 >/dev/null 2>&1; then
  PY=python3
elif command -v python >/dev/null 2>&1; then
  PY=python
else
  generic_fail
fi

if ! PR_URL=$(NM_TITLE_FILE="$TITLE" NM_BODY_FILE="$BODY" "$PY" - 2>/dev/null <<'PYTHON'
import json, os, urllib.request
repo = os.environ["NM_REPO"]
number = int(os.environ["NM_PR_NUMBER"])
token = os.environ["GITHUB_TOKEN"]
if not token or number <= 0:
    raise ValueError("missing credentials or PR")
url = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/") + f"/repos/{repo}/pulls/{number}"
request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json"})
with urllib.request.urlopen(request, timeout=30) as response:
    pr = json.load(response)
title, body, pr_url = pr["title"], pr["body"], pr["html_url"]
if not isinstance(title, str) or (body is not None and not isinstance(body, str)) or not isinstance(pr_url, str) or not pr_url:
    raise ValueError("invalid PR metadata")
open(os.environ["NM_TITLE_FILE"], "w", encoding="utf-8").write(title)
open(os.environ["NM_BODY_FILE"], "w", encoding="utf-8").write(body or "")
print(pr_url)
PYTHON
); then
  generic_fail
fi

BASE_SHA="${NM_BASE_SHA:-}"
HEAD_SHA="${NM_HEAD_SHA:-}"
if [ -n "$BASE_SHA" ] && [ -n "$HEAD_SHA" ] && command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  git fetch --quiet origin "$BASE_SHA" "$HEAD_SHA" >/dev/null 2>&1 || true
  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" != "false" ]; then
    git fetch --quiet --unshallow origin >/dev/null 2>&1 || generic_fail
  fi
  if git diff --no-ext-diff --no-textconv --no-color "${BASE_SHA}...${HEAD_SHA}" >"$DIFF" 2>/dev/null; then
    :
  else
    generic_fail
  fi
else
  generic_fail
fi

if ! git log --no-ext-diff --no-textconv --no-color --format= --root -m -p "${BASE_SHA}..${HEAD_SHA}" >>"$DIFF" 2>/dev/null; then
  generic_fail
fi
if ! git log --format=%B "${BASE_SHA}..${HEAD_SHA}" >"$COMMITS" 2>/dev/null; then
  generic_fail
fi

set +e
no-mistakes firewall github-check \
  --diff "$DIFF" \
  --title-file "$TITLE" \
  --body-file "$BODY" \
  --commits-file "$COMMITS" \
  --repo "${NM_REPO:-}" \
  --pr-number "${NM_PR_NUMBER:-0}" \
  --pr-url "$PR_URL" \
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
# Exit 1 is a Hard Rules verdict the scanner reached. Any other non-zero exit
# is the firewall failing to judge or record one, which is an error rather
# than a violation. Both fail closed, with distinct generic public text and
# conclusion outputs.
if [ "$status" -eq 1 ]; then
  echo "conclusion=failure" >> "${GITHUB_OUTPUT:-/dev/null}"
else
  echo "conclusion=error" >> "${GITHUB_OUTPUT:-/dev/null}"
fi
exit 1
