# AXI Custody Recovery Transcript

This transcript was produced by the real `no-mistakes` binary in an isolated e2e repo with the deterministic fake agent.
It reproduces a cancelled pre-push pipeline whose fix commit exists only in the local gate, then recovers custody without losing that commit.

- submitted head: `0bae93b3b0abe6f2af6928e097a0c0ac502c10c9`

## Initial run reaches review gate

Command: `no-mistakes axi run --intent guard the feature before cancellation`

Exit: `0`

```text
run: running
  intent: completed
  rebase: running
  rebase: completed
  review: running
  review: awaiting_approval
run:
  id: "01KXNFKPZHE9BYFRCZPK3E22Y7"
  branch: feature/recover-evidence
  status: running
  awaiting_agent: parked 0s
  head: 0bae93b3
  findings: 1 auto-fix
  steps[9]{step,status,findings,duration_ms}:
    intent,completed,0,0
    rebase,completed,0,124
    review,awaiting_approval,1,288
    test,pending,0,0
    document,pending,0,0
    lint,pending,0,0
    push,pending,0,0
    pr,pending,0,0
    ci,pending,0,0
gate:
  step: review
  status: awaiting_approval
  summary: found one issue
  risk: medium
  note: "Review auto-fix is disabled by default (`auto_fix.review: 0`; a repo or global `auto_fix.review > 0` override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed."
  findings[1]{id,severity,file,action,description}:
    sync-1,warning,feature.txt,auto-fix,unsafe value needs validation
help[6]: Run `no-mistakes axi respond --action approve` to accept this step and continue,Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself),Run `no-mistakes axi respond --action skip` to skip this step,Run `no-mistakes axi logs --step review --full` to read the full step log,"A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a `gate:`, respond; loop until an `outcome:`.","Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present. Never abort-and-restart, reset, or replace the branch in a way that drops prior gate-fix commits."
```

## Pipeline applies review fix in gate

Command: `no-mistakes axi respond --action fix --findings sync-1`

Exit: `0`

```text
run: running
  intent: completed
  rebase: completed
  review: fix_review
run:
  id: "01KXNFKPZHE9BYFRCZPK3E22Y7"
  branch: feature/recover-evidence
  status: running
  awaiting_agent: parked 0s
  head: 2658d96b
  findings: 1 auto-fix
  steps[9]{step,status,findings,duration_ms}:
    intent,completed,0,0
    rebase,completed,0,124
    review,fix_review,1,388
    test,pending,0,0
    document,pending,0,0
    lint,pending,0,0
    push,pending,0,0
    pr,pending,0,0
    ci,pending,0,0
branch_sync:
  state: pipeline_owned
  changed: false
  local:
    branch: feature/recover-evidence
    head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    clean: true
  pipeline:
    run: "01KXNFKPZHE9BYFRCZPK3E22Y7"
    status: running
    phase: pre_push
    submitted_head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    current_head: 2658d96bd4fc08918468a199033777530e42aaad
    pushed_head: ""
    pushed_at: 0
    push_generation: 0
  target:
    kind: ""
    remote: origin
    url: /var/folders/5x/4nqprlbx0518k3ybcb1sz6gr0000gn/T/nm-e2e-3617249042/upstream.git
    ref: ""
  remote:
    observed_head: ""
    freshness: pipeline_push
    observed_at: 0
  relation: unknown
  safety: blocked_pipeline_owned
  pr_state: none
  note: the pipeline head has moved but has not been successfully pushed; do not make local follow-up commits yet
gate:
  step: review
  status: fix_review
  summary: found one issue
  risk: medium
  note: "Review auto-fix is disabled by default (`auto_fix.review: 0`; a repo or global `auto_fix.review > 0` override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed."
  findings[1]{id,severity,file,action,description}:
    sync-1,warning,feature.txt,auto-fix,unsafe value needs validation
help[6]: Run `no-mistakes axi respond --action approve` to accept this step and continue,Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself),Run `no-mistakes axi respond --action skip` to skip this step,Run `no-mistakes axi logs --step review --full` to read the full step log,"A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a `gate:`, respond; loop until an `outcome:`.","Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present. Never abort-and-restart, reset, or replace the branch in a way that drops prior gate-fix commits."
```

## Abort before push stage

Command: `no-mistakes axi abort`

Exit: `0`

```text
aborted: true
run: "01KXNFKPZHE9BYFRCZPK3E22Y7"
branch: feature/recover-evidence
help[1]: "Run `no-mistakes axi sync --check` before any local follow-up commit - a cancelled run can leave unpublished pipeline commits preserved in the local gate, and the check offers the guarded custody recovery"
```

## Preserved State Before Recovery

- run status: `cancelled`
- gate preserved head: `2658d96bd4fc08918468a199033777530e42aaad`
- operator worktree head: `0bae93b3b0abe6f2af6928e097a0c0ac502c10c9`

## Stranded branch points at recover_custody

Command: `no-mistakes axi sync --check`

Exit: `exit status 1`

```text
branch_sync:
  state: pipeline_owned
  changed: false
  local:
    branch: feature/recover-evidence
    head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    clean: true
  pipeline:
    run: "01KXNFKPZHE9BYFRCZPK3E22Y7"
    status: cancelled
    phase: pre_push
    submitted_head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    current_head: 2658d96bd4fc08918468a199033777530e42aaad
    pushed_head: ""
    pushed_at: 0
    push_generation: 0
  target:
    kind: ""
    remote: origin
    url: /var/folders/5x/4nqprlbx0518k3ybcb1sz6gr0000gn/T/nm-e2e-3617249042/upstream.git
    ref: ""
  remote:
    observed_head: ""
    freshness: pipeline_push
    observed_at: 0
  relation: unknown
  safety: blocked_pipeline_owned_recoverable
  pr_state: none
  note: the run finished cancelled with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit
  next_action:
    code: recover_custody
    command: no-mistakes axi sync --recover
error: the run finished cancelled with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit
help[2]: Run `no-mistakes axi sync --recover`,Run `no-mistakes rerun` instead to resume validating the preserved pipeline head
```

## Recover custody

Command: `no-mistakes axi sync --recover`

Exit: `0`

```text
branch_sync:
  state: custody_returned
  changed: true
  recovered: true
  local:
    branch: feature/recover-evidence
    head: 2658d96bd4fc08918468a199033777530e42aaad
    clean: true
  pipeline:
    run: "01KXNFKPZHE9BYFRCZPK3E22Y7"
    status: cancelled
    phase: ""
    submitted_head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    current_head: 2658d96bd4fc08918468a199033777530e42aaad
    pushed_head: ""
    pushed_at: 0
    push_generation: 0
  target:
    kind: ""
    remote: origin
    url: /var/folders/5x/4nqprlbx0518k3ybcb1sz6gr0000gn/T/nm-e2e-3617249042/upstream.git
    ref: ""
  remote:
    observed_head: ""
    freshness: pipeline_push
    observed_at: 0
  relation: equal
  safety: custody_returned
  pr_state: none
  next_action:
    code: run_pipeline
    command: "no-mistakes axi run --intent \"<what the user set out to accomplish>\""
help[1]: "Run `no-mistakes axi run --intent \"<what the user set out to accomplish>\"`"
```

## Preserved State After Recovery

- operator worktree head: `2658d96bd4fc08918468a199033777530e42aaad`
- local recovery anchor: `2658d96bd4fc08918468a199033777530e42aaad`

## Recovered branch is no longer blocked

Command: `no-mistakes axi sync --check`

Exit: `0`

```text
branch_sync:
  state: custody_returned
  changed: false
  local:
    branch: feature/recover-evidence
    head: 2658d96bd4fc08918468a199033777530e42aaad
    clean: true
  pipeline:
    run: "01KXNFKPZHE9BYFRCZPK3E22Y7"
    status: cancelled
    phase: ""
    submitted_head: 0bae93b3b0abe6f2af6928e097a0c0ac502c10c9
    current_head: 2658d96bd4fc08918468a199033777530e42aaad
    pushed_head: ""
    pushed_at: 0
    push_generation: 0
  target:
    kind: ""
    remote: origin
    url: /var/folders/5x/4nqprlbx0518k3ybcb1sz6gr0000gn/T/nm-e2e-3617249042/upstream.git
    ref: ""
  remote:
    observed_head: ""
    freshness: pipeline_push
    observed_at: 0
  relation: equal
  safety: custody_returned
  pr_state: none
  next_action:
    code: run_pipeline
    command: "no-mistakes axi run --intent \"<what the user set out to accomplish>\""
help[1]: "Run `no-mistakes axi run --intent \"<what the user set out to accomplish>\"`"
```

## Follow-up Commit

- rescope head: `4201a21c204b32d1f8b6f82d44605034928d2e0f`
- preserved commit remains an ancestor: `true`

## Fresh run starts from recovered custody

Command: `no-mistakes axi run --intent validate the rescope on top of recovered commits`

Exit: `0`

```text
run: running
  intent: completed
  rebase: running
  rebase: completed
  review: awaiting_approval
run:
  id: "01KXNFKRT4GJ3H3RDZ8870WPKG"
  branch: feature/recover-evidence
  status: running
  awaiting_agent: parked 0s
  head: 4201a21c
  findings: 1 auto-fix
  steps[9]{step,status,findings,duration_ms}:
    intent,completed,0,1
    rebase,completed,0,121
    review,awaiting_approval,1,26
    test,pending,0,0
    document,pending,0,0
    lint,pending,0,0
    push,pending,0,0
    pr,pending,0,0
    ci,pending,0,0
gate:
  step: review
  status: awaiting_approval
  summary: found one issue
  risk: medium
  note: "Review auto-fix is disabled by default (`auto_fix.review: 0`; a repo or global `auto_fix.review > 0` override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed."
  findings[1]{id,severity,file,action,description}:
    sync-1,warning,feature.txt,auto-fix,unsafe value needs validation
help[6]: Run `no-mistakes axi respond --action approve` to accept this step and continue,Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself),Run `no-mistakes axi respond --action skip` to skip this step,Run `no-mistakes axi logs --step review --full` to read the full step log,"A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a `gate:`, respond; loop until an `outcome:`.","Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present. Never abort-and-restart, reset, or replace the branch in a way that drops prior gate-fix commits."
```
