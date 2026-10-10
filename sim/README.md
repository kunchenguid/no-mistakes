# Gate-decline reproduction

`../scripts/repro-gate-declines.sh` reproduces the defect this change fixes, end to end and without
any of the operator's context: a `respond --action fix` that leaves out findings the gate still
carries used to record those findings as declined.

```
# against a checkout of main (shows the defect)
scripts/repro-gate-declines.sh <path-to-a-main-checkout> /tmp/declines-before

# against a checkout of this branch (shows the fix)
scripts/repro-gate-declines.sh <path-to-this-branch-checkout> /tmp/declines-after
```

The script builds the tool and the repository's scripted agent from the checkout under test, creates
a scratch project (a `deploy.sh` / `snapshot.sh` with obvious flaws), drives it through the real
pipeline in an isolated environment, and answers the gates the way a person does:

1. the first review reports five findings; the operator asks for every one of them
   (`axi respond --action fix --findings r1,r2,r3,r4,r5`);
2. the rereview reports two more findings without claiming coverage of the earlier files, so the
   first five are still carried in the next gate: it shows seven findings;
3. the operator asks for only the two new ones (`--findings r6,r7`).

Each run leaves its full transcript in `<output-dir>/transcript.txt`, and the two captured runs are
kept here:

- `repro-transcript-before.txt` - the run on main, where round 2's omission is recorded as a decline
  and the next review turn is told the five round-1 fixes are under `user_chose_to_ignore`;
- `repro-transcript-after.txt` - the run with this change, where the same response keeps them and
  reports them under `kept`.

## What the fixture does and does not show

- The review turns are scripted: the findings, their IDs, severities and line numbers come from the
  scenario file rather than a real reviewer. The line numbers name lines of the fixture's own files,
  and the fixer applies the edits it claims in both `deploy.sh` and `snapshot.sh`; the script asserts
  that every claimed file really changed, so a claim that is not applied fails the run instead of
  making the accounting look better than the code.
- What it demonstrates end to end is the decline rendering: the same response, the same pipeline, and
  the difference is only in how the omitted findings are recorded and rendered to the next review
  turn.

## Isolation

- Its own `HOME` and its own `NM_HOME` under the output directory: the daemon, socket, database,
  worktrees, and logs are fresh, and nothing in the operator's `~/.no-mistakes`, projects, or git
  configuration is read or written.
- Its own bare origin and gate: no forge, no network service, and no credentials.
- A scripted agent (the repository's own `cmd/fakeagent`) answers every turn from a scenario file, so
  runs are deterministic and cost nothing.
- `NM_TEST_START_DAEMON=1` starts the daemon as a detached child process instead of installing a
  system service, and the script stops that daemon when it finishes.

The only external requirement is a Go toolchain, used to build the checkout under test and the
scripted agent from it.
