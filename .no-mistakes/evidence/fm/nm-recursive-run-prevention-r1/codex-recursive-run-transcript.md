# Recursive run prevention - Codex end-to-end transcript

Captured from a real Codex validation-step child while running:

```text
bash scripts/e2e.sh -tags=e2e -count=1 -timeout 180s \
  -run '^TestGateStepCannotStartRecursivePipeline/codex$' ./internal/e2e
```

The isolated journey created an ordinary outer run, entered its `document`
validation step, and invoked the public CLI from that agent process. The focused
test passed after also verifying that the isolated database still contained
only the outer repo and run, the attempted direct-push ref did not exist, and no
recursive gate artifact was created.

## Read-only commands remain available

```text
=== readonly-status
run:
  id: "01KY8XM0XZANA0R7DXXEGTCYJV"
  branch: feature/recursive-incident
  status: running
  head: 98e3aa48
  findings: 1 info
  steps[9]{step,status,findings,duration_ms}:
    intent,skipped,0,42
    rebase,completed,0,122
    review,completed,1,427
    test,completed,0,29
    document,running,0,0
    lint,pending,0,0
    push,pending,0,0
    pr,pending,0,0
    ci,pending,0,0
exit: 0

=== readonly-logs
step: document
run: "01KY8XM0XZANA0R7DXXEGTCYJV"
lines: 3 total
log[3]{line}:
  "housekeeping: updating documentation and linting in one pass..."
  ""
  codex started pid=34188
exit: 0

=== readonly-help
Usage:
  no-mistakes axi run [flags]

Flags:
  -h, --help            help for run
      --intent string   what the user set out to accomplish (not a description of the diff); used instead of inferring it from transcripts (required to start a run)
      --skip string     comma-separated pipeline steps to skip
  -y, --yes             auto-resolve every gate (fix findings, then accept) until a decision point or outcome
exit: 0
```

## Exact incident sequence is refused

Both `no-mistakes init` and `no-mistakes axi run --yes` returned the same
structured response. The run and phase are included because authenticated
ancestry made them safely known.

```text
=== exact-init-marker-present
error:
  code: nested_gate_context
  message: refusing pipeline control from an active no-mistakes validation step
  run: "01KY8XM0XZANA0R7DXXEGTCYJV"
  phase: document
note: "The enclosing executor owns validation, push, PR, and CI. This step must return only its assigned phase."
allowed[3]: no-mistakes axi status --run 01KY8XM0XZANA0R7DXXEGTCYJV,no-mistakes axi logs --run 01KY8XM0XZANA0R7DXXEGTCYJV --step <phase>,no-mistakes doctor
help[1]: "Return control to the outer executor; do not initialize, start, reattach, rerun, respond to, synchronize, abort, or eject a pipeline from this step."
exit: 1

=== exact-axi-run-yes-marker-present
error:
  code: nested_gate_context
  message: refusing pipeline control from an active no-mistakes validation step
  run: "01KY8XM0XZANA0R7DXXEGTCYJV"
  phase: document
note: "The enclosing executor owns validation, push, PR, and CI. This step must return only its assigned phase."
allowed[3]: no-mistakes axi status --run 01KY8XM0XZANA0R7DXXEGTCYJV,no-mistakes axi logs --run 01KY8XM0XZANA0R7DXXEGTCYJV --step <phase>,no-mistakes doctor
help[1]: "Return control to the outer executor; do not initialize, start, reattach, rerun, respond to, synchronize, abort, or eject a pipeline from this step."
exit: 1
```

## Marker, cwd, concurrency, and force do not bypass the guard

Each of the following transcript sections returned
`error.code: nested_gate_context` and `exit: 1`:

```text
=== rerun
=== respond
=== sync
=== recover
=== abort
=== eject
=== force-daemon-stop
=== init-marker-removed
=== changed-cwd-marker-removed
=== concurrent-init-1
=== concurrent-init-2
```

The two concurrent attempts each independently returned the structured refusal.
`force-daemon-stop` invoked `no-mistakes daemon stop --force`, demonstrating
that force does not bypass validation-step containment.

## Direct gate push is refused before ref mutation

```text
=== direct-gate-push
remote: no-mistakes: gate push refused before ref mutation:
remote: error:
remote:   code: nested_gate_context
remote:   message: refusing pipeline control from an active no-mistakes validation step
remote:   run: "01KY8XM0XZANA0R7DXXEGTCYJV"
remote:   phase: document
remote: note: "The enclosing executor owns validation, push, PR, and CI. This step must return only its assigned phase."
remote: allowed[3]: no-mistakes axi status --run 01KY8XM0XZANA0R7DXXEGTCYJV,no-mistakes axi logs --run 01KY8XM0XZANA0R7DXXEGTCYJV --step <phase>,no-mistakes doctor
remote: help[1]: "Return control to the outer executor; do not initialize, start, reattach, rerun, respond to, synchronize, abort, or eject a pipeline from this step."
 ! [remote rejected] HEAD -> incident-direct-bypass (pre-receive hook declined)
error: failed to push some refs
exit: 1
```

Final command result:

```text
ok github.com/kunchenguid/no-mistakes/internal/e2e 4.169s
```
