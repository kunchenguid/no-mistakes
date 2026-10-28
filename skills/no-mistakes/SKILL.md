---
name: no-mistakes
description: Validate your code changes through the no-mistakes pipeline - automated code review, tests, lint, docs, push, PR, and CI - before they reach the configured push target. Use when the user asks to run no-mistakes, gate or ship or validate their changes, push safely, asks you to do a task and then validate it, or invokes /no-mistakes.
user-invocable: true
---

# no-mistakes

`no-mistakes` is a local gate that validates your code changes through a pipeline
(intent, rebase, review, test, document, lint, push, PR, CI) before they reach
the configured push target. You drive it through the `no-mistakes axi` command family, which prints
machine-readable [TOON](https://toonformat.dev) to stdout and progress to stderr.


## Active validation-step boundary

A no-mistakes validation-step agent is already inside an active outer run. It
must inspect, fix, and return only its assigned phase. It must never initialize,
start, reattach, rerun, respond to, synchronize, abort, eject, or directly push
a no-mistakes pipeline. Delivery requirements in user intent remain
acceptance context, but the outer executor alone performs the other validation,
push, PR, and CI phases.

`NO_MISTAKES_GATE` is fast diagnostic evidence, not authorization by
itself. The runtime combines managed Git identity with authenticated process
ancestry. If a pipeline-control command returns
`error.code: nested_gate_context`, stop immediately and
return control to the outer executor. Safe inspection remains available through
`no-mistakes axi status`, `no-mistakes axi logs`, help, and
`no-mistakes doctor`.


When the user invokes `/no-mistakes`, report the outcome at the end. If the user
asks for something specific, translate that request into the matching `axi run`
flags yourself - for example, "skip the lint step" becomes `--skip=lint`. Run
`no-mistakes axi run --help` to see the available flags.

## Before you start

Use a committed feature branch in an initialized repository. For task-first
invocations, implement and commit the user's task before validating it. Read
[getting started](references/getting-started.md) for scope, test-quality,
preconditions, agent availability, and the required explicit intent.

The daemon is shared with other runs. Never stop, restart, or update it to
unstick your run. Inspect with `no-mistakes daemon status` and
`no-mistakes axi status`, and report a daemon failure to the operator.

## Validate and decide

1. Run `no-mistakes axi` to inspect the current branch. Leave other
   branches' runs alone. Reattach a matching in-flight run with
   `no-mistakes axi run`; respond to its parked gate instead of starting over.
2. Start new validation with
   `no-mistakes axi run --intent '<what the user set out to accomplish>'`.
   Calls can take several minutes and default to `--wait 8m`.
   Background the call if your harness requires it. A wait expiring is not
   a failed run or proof the daemon died. Inspect status and reattach.
3. Read every return. A `gate:` waits for your response and never advances
   on its own. Read [gate procedures](references/gates.md) before responding,
   including finding selection, review questions, exceptions, and outcomes.
   For eligible mechanical findings use
   `no-mistakes axi respond --action fix --findings <ids>`.
   Use the returned `help` and live command help for version-matched flags.
4. Continue until an `outcome:`. Normal decision gates are exit 0 stops.
   `checks-passed` means CI readiness, not merge. Stop driving and report
   the PR and any fixes, exceptions, or missing evidence. Do not wait for merge.
   A generic empty check list does not prove readiness.
5. Before a post-pipeline local commit or fresh run, read `branch_sync`.
   Follow its exact next action and read [recovery](references/recovery.md)
   before sync, retry, rerun, custody recovery, or updating a stale PR.
   Commit follow-up work on top so every pipeline fix commit remains present.

While a run is active, the pipeline owns code changes. Decide and respond at
gates. Never hand-edit fixes, abort-and-restart, reset, or replace the branch
to get around a finding or drop pipeline fixes.

## Escalate ask-user findings

`auto-fix` and `no-op` findings can be decided on your own judgment.
For `ask-user`, stop and relay every finding's id, file, and full
description verbatim to the user. Do not approve, fix, or skip it yourself.
Translate their decision into the response, passing their guidance through
`--instructions` when fixing.

Only explicit consent to drive the whole run unattended authorizes
`--yes` to resolve ordinary ask-user findings automatically. Even then,
protected-path refusals, open review questions, unreadable question history,
and unvalidated Test work require an operator. Read the
[escalation exceptions](references/gates.md#escalate-ask-user-findings)
before acting. Review questions need `axi answer`, never a verdict.

## On-demand references

- [Getting started](references/getting-started.md): invocation modes, task
  scope, test quality, prerequisites, intent input.
- [Gate procedures](references/gates.md): finding actions, complete coverage,
  review answers, Test exceptions, unattended consent, outcome reporting.
- [Recovery](references/recovery.md): failed runs, retries, branch custody,
  synchronization, preserved fixes, and stale CI monitors.
- [Commands and output](references/commands.md): inspection commands,
  run scoping, TOON fields, liveness, exit codes, and a gate example.
Read the applicable reference when that situation occurs. Paths are relative
to this skill directory, including after installation by init.
