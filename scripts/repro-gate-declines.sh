#!/usr/bin/env bash
# Reproduce a partial gate response being recorded as a decline, end to end.
#
# A scratch project is driven through the real pipeline in a fully isolated
# environment: its own HOME, its own NM_HOME (daemon socket, database, logs),
# its own bare origin, and a scripted agent (the repository's cmd/fakeagent),
# so nothing of the operator's state is read or written and no credentials,
# network service, or API key is involved.
#
# The journey is the one a human driver makes at a review gate:
#   round 1: the review reports five findings; the operator asks for all five.
#   round 2: the rereview reports two more findings while the first five are
#            still carried in the gate; the operator asks for only the two new
#            ones.
#
# Usage: repro-gate-declines.sh <no-mistakes-checkout> <output-dir>
#
# Run it against a checkout of the branch under review and against a checkout
# of main; the last section of the output states what the gate recorded.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <no-mistakes-checkout> <output-dir>" >&2
  exit 2
fi

SRC=$(cd "$1" && pwd)
OUT=$(mkdir -p "$2" && cd "$2" && pwd)
WORK="$OUT/work"

# Keep a readable transcript next to the run's scratch state.
exec > >(tee "$OUT/transcript.txt") 2>&1

rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/home" "$WORK/nm-home" "$WORK/project" "$WORK/origin.git"

BIN="$WORK/bin"
SIM_HOME="$WORK/home"
NM_HOME="$WORK/nm-home"
PROJECT="$WORK/project"
AGENT_LOG="$WORK/agent.log"
SCENARIO="$WORK/scenario.yaml"

echo "# repro: no-mistakes gate declines from a partial response"
echo
echo "- checkout under test: $SRC"
echo "- commit: $(git -C "$SRC" rev-parse --short HEAD) $(git -C "$SRC" log -1 --format=%s)"
echo "- isolated home: $NM_HOME (the operator's own ~/.no-mistakes is never touched)"
echo "- scratch project: $PROJECT"
echo

echo "## Build the tool and the scripted agent from the checkout"
(cd "$SRC" && go build -o "$BIN/no-mistakes" ./cmd/no-mistakes && go build -o "$BIN/fakeagent" ./cmd/fakeagent)
echo "built $BIN/no-mistakes and $BIN/fakeagent"
echo

# The daemon resolves its PATH through a login shell, so the fake agent names
# must be visible from the simulated home's shell start-up files too.
for rc in .profile .bash_profile .zshenv .zprofile; do
  printf 'export PATH=%s:"$PATH"\n' "'$BIN'" > "$SIM_HOME/$rc"
done

# The fake agent answers as claude (dispatched by argv[0]) and stands in for
# gh/tea, so no real forge CLI is ever consulted.
for name in claude gh tea; do
  ln -s "$BIN/fakeagent" "$BIN/$name"
done

cat > "$NM_HOME/config.yaml" <<EOF
agent: claude
log_level: debug
agent_path_override:
  claude: $BIN/claude
auto_fix:
  rebase: 0
  lint: 0
  test: 0
  review: 0
  document: 0
  ci: 0
EOF

# The isolated environment every no-mistakes invocation runs in.
nm() {
  env -i \
    PATH="$BIN:/usr/bin:/bin" \
    HOME="$SIM_HOME" \
    NM_HOME="$NM_HOME" \
    SHELL="${SHELL:-/bin/bash}" \
    FAKEAGENT_LOG="$AGENT_LOG" \
    FAKEAGENT_SCENARIO="$SCENARIO" \
    NM_TEST_START_DAEMON=1 \
    NM_TEST_DAEMON_START_TIMEOUT=45s \
    NO_MISTAKES_TELEMETRY=off \
    NO_MISTAKES_NO_UPDATE_CHECK=1 \
    LANG=C.UTF-8 \
    "$BIN/no-mistakes" "$@"
}

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  # The simulated run parks at the review gate on purpose, and a plain stop
  # refuses while any run is active, so force it and then verify: this daemon is
  # this simulation's own, and nothing else may be left running.
  nm daemon stop --force >/dev/null 2>&1 || true
  if [ -f "$NM_HOME/daemon.pid" ]; then
    pid=$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$NM_HOME/daemon.pid" | head -1)
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      for _ in 1 2 3 4 5; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.3
      done
    fi
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | grep -qF "$NM_HOME"; then
      kill -TERM "$pid" 2>/dev/null || true
      sleep 1
      kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  if pgrep -f "$NM_HOME" >/dev/null 2>&1; then
    echo "warning: a process still references the isolated home $NM_HOME" >&2
  fi
  echo
  echo "(isolated daemon stopped; scratch project and logs left in $WORK)"
  exit "$status"
}
trap cleanup EXIT

# The scratch project: a deploy script with obvious, deterministic flaws.
git init -q --bare --initial-branch=main "$WORK/origin.git"
git init -q --initial-branch=main "$PROJECT"
git -C "$PROJECT" config user.name "Repro Operator"
git -C "$PROJECT" config user.email "repro@example.invalid"
git -C "$PROJECT" config commit.gpgsign false
cat > "$PROJECT/README.md" <<'EOF'
# example-service

A tiny service with a deploy script.
EOF
git -C "$PROJECT" add README.md
git -C "$PROJECT" commit -q -m "initial commit"
git -C "$PROJECT" remote add origin "$WORK/origin.git"
git -C "$PROJECT" push -q -u origin main

git -C "$PROJECT" checkout -q -b harden-deploy
cat > "$PROJECT/deploy.sh" <<'EOF'
#!/usr/bin/env bash
# deploy the staged build
./build.sh --allow-dirty
tar -czf snapshot.tar.gz /srv/app
cat <<HEREDOC > compose.yaml
image: example-service:${TAG}
HEREDOC
curl "https://example.invalid/healthz" || true
echo "deployed ${TAG}"
EOF
cat > "$PROJECT/snapshot.sh" <<'EOF'
#!/usr/bin/env bash
# snapshot the release artifacts
tar -czf snapshot.tar.gz /srv/app
git add snapshot.tar.gz
EOF
git -C "$PROJECT" add deploy.sh snapshot.sh
git -C "$PROJECT" commit -q -m "add deploy and snapshot scripts"

cd "$PROJECT"
nm init >/dev/null

# The scripted reviewer: five findings in the first pass, two more in the
# rereview (which claims no coverage of the earlier files, so the first five
# stay carried in the next gate), and a fix turn that actually rewrites the
# deploy script.
cat > "$SCENARIO" <<'EOF'
actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: r6
          severity: error
          file: snapshot.sh
          line: 3
          description: the snapshot tarball is written into the repository root instead of the artifacts directory
          action: ask-user
          review_scope: source
        - id: r7
          severity: warning
          file: deploy.sh
          line: 9
          description: cleanup never runs when the deploy fails, leaving the staging directory behind
          action: auto-fix
          review_scope: source
      summary: two more findings in the deploy path
      risk_level: medium
      risk_rationale: the rereview found two more issues
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: deploy.sh
        new: |
          #!/usr/bin/env bash
          set -euo pipefail
          : "${TAG:?set TAG}"
          ./build.sh
          tar -czf /var/tmp/snapshot.tar.gz /srv/app
          printf 'image: example-service:%s\n' "$TAG" > compose.yaml
          curl -fsS "https://example.invalid/healthz"
          echo "deployed ${TAG}"
      - path: snapshot.sh
        new: |
          #!/usr/bin/env bash
          set -euo pipefail
          mkdir -p artifacts
          tar -czf artifacts/snapshot.tar.gz /srv/app
    structured:
      findings: []
      summary: applied the selected fixes
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: r1
          severity: error
          file: deploy.sh
          line: 3
          description: the allow-dirty bypass lets a dirty working tree ship uncommitted files
          action: ask-user
          review_scope: source
        - id: r2
          severity: warning
          file: deploy.sh
          line: 1
          description: set -u is missing so an unset TAG expands to an empty value
          action: ask-user
          review_scope: source
        - id: r3
          severity: warning
          file: deploy.sh
          line: 5
          description: the compose heredoc expands variables because its delimiter is unquoted
          action: auto-fix
          review_scope: source
        - id: r4
          severity: error
          file: snapshot.sh
          line: 3
          description: the snapshot tarball is written into the repository and committed with git add
          action: ask-user
          review_scope: source
        - id: r5
          severity: warning
          file: deploy.sh
          line: 8
          description: the health check exit status is ignored so a failed deploy reports success
          action: auto-fix
          review_scope: source
      summary: five findings
      risk_level: high
      risk_rationale: the deploy path can ship uncommitted files and report success after a failed health check
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: nothing else to report
      risk_scope: source-or-external
EOF

echo "## Round 1: run the pipeline, then ask for every finding"
echo
# The submitted head, so the fix commits can be told apart from the branch's own
# content when the script verifies what the fixer actually applied.
SUBMITTED=$(git -C "$PROJECT" rev-parse HEAD)
nm axi run --intent "Harden the deploy script: ship only committed files, fail loudly on a bad environment, and keep release artifacts out of the repository" --wait 3m
echo
echo "\$ no-mistakes axi respond --action fix --findings r1,r2,r3,r4,r5"
nm axi respond --action fix --findings r1,r2,r3,r4,r5 --wait 3m
echo

echo "## Round 2: ask for only the two new findings"
echo
echo "\$ no-mistakes axi respond --action fix --findings r6,r7"
nm axi respond --action fix --findings r6,r7 --wait 3m
echo

echo "## What the fixer actually applied"
echo
# The scenario's fixer claims findings in deploy.sh and snapshot.sh, and the
# gate then reports them as chosen for fix. A claim that is not applied would
# make the run's accounting look better than the code, so assert that every file
# the fixes claim really changed between the submitted head and the run's head.
# The run's worktree is $NM_HOME/worktrees/<repo>/<run> and its `.git` is a file,
# so the first `.git` under that tree marks the checkout to inspect. A defect is
# recorded rather than exited on, so the script still stops its own daemon.
worktree=$(find "$NM_HOME/worktrees" -maxdepth 3 -name .git 2>/dev/null | head -1 | xargs -r dirname)
fixture_defect=""
if [ -z "$worktree" ]; then
  fixture_defect="no run worktree under $NM_HOME/worktrees; cannot verify the applied fixes"
else
  changed=$(git -C "$worktree" diff --name-only "$SUBMITTED"..HEAD 2>/dev/null | sort)
  echo "files the pipeline's fix rounds changed:"
  printf '%s\n' "$changed" | sed 's/^/  /'
  missing=""
  for file in deploy.sh snapshot.sh; do
    printf '%s\n' "$changed" | grep -qx "$file" || missing="$missing $file"
  done
  if [ -n "$missing" ]; then
    fixture_defect="the fixes claim files that never changed:$missing"
  else
    echo "every file the fixes claim (deploy.sh, snapshot.sh) really changed"
  fi
fi
if [ -n "$fixture_defect" ]; then
  echo "FIXTURE DEFECT: $fixture_defect" >&2
fi
echo

echo "## What the next review turn was told about round 2"
echo
# The fake agent logs every prompt it receives; the round-2 block of the
# round-history section is what tells a later review turn what the operator
# decided. Newlines are JSON-escaped in the log line.
round2=$(grep -h 'Round 2 (auto_fix)' "$AGENT_LOG" | tail -1 || true)
if [ -z "$round2" ]; then
  echo "no review turn carried a round-2 block; the run did not reach it"
  exit 1
fi
echo "the section's own instructions to the reviewer:"
printf '%s\n' "$round2" | grep -o 'Previous rounds for this step.*' | sed 's/\\n/\
/g; s/\\"/"/g' | sed -n '2p' | fold -s -w 110 | sed 's/^/  /'
echo
# The round-2 block runs until the next section heading in the prompt.
block=$(printf '%s\n' "$round2" | grep -o 'Round 2 (auto_fix).*' | sed 's/\\n/\
/g; s/\\"/"/g' | awk 'NR > 1 && (/^Round [0-9]/ || /^Fix-round provenance:/ || /^User intent/ || /^Workspace/) { exit } { print }')
echo "round 2's block:"
printf '%s\n' "$block" | sed 's/^/  /'
echo

echo "## Verdict"
echo
declined=$(printf '%s\n' "$block" | awk '/^user_chose_to_ignore:/{seen=1;next} seen && /^  - /{n++} seen && !/^  - /{exit} END{print n+0}')
if [ "$declined" -gt 0 ]; then
  echo "DEFECT PRESENT: round 2's omission was recorded as a decline. The five"
  echo "findings fixed in round 1 are rendered under user_chose_to_ignore"
  echo "($declined entries), and the next review turn is told not to implement"
  echo "them, while the same prompt also tells it not to revert what the user"
  echo "chose under user_chose_to_fix."
else
  echo "round 2's omission kept the five round-1 fixes: nothing is rendered as"
  echo "declined, and the response reported them under kept (see the recorded:"
  echo "block above the round-2 gate)."
fi


if [ -n "$fixture_defect" ]; then
  exit 1
fi
