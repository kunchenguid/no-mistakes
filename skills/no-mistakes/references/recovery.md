
# Recovery

`no-mistakes rerun` keeps its existing head selection: the gate head, or the
latest terminal run's verified unpublished preserved head while custody remains
outstanding. If a known clean caller `HEAD` differs from that selected head,
it refuses before starting or superseding any run and reports both full SHAs.
It never substitutes the caller head or moves either branch to make them match.
On refusal, inspect `no-mistakes axi status` and follow the custody guidance
below. Dirty callers and callers without clean-head evidence retain existing
selection behavior.

When a `failed` outcome came from the agent invocation itself - a provider
processing error, a usage or quota limit, a transport failure, or the agent
process exiting - in Test or an earlier step, and the run never pushed,
`no-mistakes axi retry --run <id>` continues that run instead of repeating it:
a new run starts at the preserved head, carries the steps before the failed
one, and restarts the failed step from the beginning, so a failed Review
reviews the resulting head in full before Test. First adopt the preserved head
with the reported `no-mistakes axi sync --recover` so your clean `HEAD` is
exactly that head. Retry refuses, changing nothing, and names the failed proof
whenever continuing is not provably safe - a failure about the change itself,
a moved or dirty branch, another run, or changed configuration (repository
settings, the agent, or its model since the carried Review ran); then use
`rerun` or address the failure. Like `respond`, it blocks until the next
`gate:` or outcome.

Before any post-pipeline local commit or fresh run, read the structured `branch_sync` object returned by AXI home, status, or a drive result.
Only when its `next_action.code` is `sync`, run `no-mistakes axi sync` first.
That guarded sync may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics; genuine divergence stays blocked and names `git checkout --detach <pushed-sha>`, which moves no branch ref.
If it reports `next_action.code` is `continue_active_run`, the pipeline still owns the branch: run the reported command, keep driving the active run, and do not make local follow-up commits.
When `next_action.code` is `recover_custody`, run its exact `next_action.command` rather than reconstructing one. That is `no-mistakes axi sync --recover` to take a still-available preserved pipeline head, or `no-mistakes axi sync --recover --keep-local` in two keep-local cases: when an accessible gate confirms the verified preserved head is missing and you are explicitly discarding those unpublished commits, or when a bound archive proves divergent later work remains preserved while recovery keeps the branch at the exact reported required head and never selects, merges, or replays the archive. Do not substitute plain `--recover` or `rerun` for a reported keep-local action. `no-mistakes rerun` can resume validating a still-available ordinary preserved head instead, subject to the clean-head check above.
Ordinary recovery takes that head by fast-forward, or by adopting a diverged preserved head proven to carry every local change - the ordinary result of the pipeline rebasing your commits onto a newer base - after anchoring your pre-recovery head under `refs/no-mistakes/recover-local/<run>`.
The ordinary containment proof is deliberately narrow, so a rebase whose fix rounds also rewrote your own lines refuses instead of being adopted: when nothing can tell a deliberate pipeline fix from a dropped change, the decision is yours.
When a run failed in a step after Test without touching the code (a Document or Lint fetch error) and its recorded head was never verified, recovery still returns custody at that head once your clean checkout sits exactly on it and the gate proves the head is the reviewed result plus the pipeline's own later commits on top of the live gate branch; recovery never moves a behind worktree onto an unverified head, so check that exact head out first.
When `next_action.code` is `recover_remote_rewritten`, the configured push target was force-rewritten outside the pipeline after a terminal run: run exact `no-mistakes axi sync --recover`. It re-verifies the live target, anchors the superseded pipeline head under `refs/no-mistakes/recover-rewritten/<run>/<generation>`, and rebinds only the recorded push binding to the verified live head; it never moves your branch, the gate, or the remote. It refuses `--keep-local`, a live head or target that changes during recovery, a head it cannot anchor, and a merged or closed PR. Afterwards follow the ordinary `next_action` it reports.
When `next_action.code` is `adopt_published`, a custody-returned branch was rebased after its gate lane stopped moving: run `no-mistakes axi sync --adopt-published`. It verifies the configured push target already has the exact rebased local head, preserves the old lane head, and updates only that stale gate lane. If the target differs or changes during verification, it refuses without replacing the lane.
A `branch_sync.state` of `user_owned` means the run went terminal before changing the submitted head and cancellation released the branch: the exact branch and head are yours and immediately usable for whichever delivery path is authorized - no sync action is needed, and a repeated `--recover` there is a harmless no-op.
A dirty worktree, or divergence that cannot be proven contained, makes the recovery refuse with explicit choices; `--keep-local` keeps your current head while the preserved commits stay anchored under `refs/no-mistakes/recover/<run>`. The same flag is the recovery when an accessible gate confirms that the verified preserved head is missing and recovery refs are compatible: it returns custody at the current local head without requiring that object.
If synchronization is blocked, process that structured state instead of improvising reset, stash, merge, rebase, force, or branch replacement.
After synchronization, commit the follow-up on top and re-run `no-mistakes axi run --intent "..."` with the original user intent.
This preserves every prior gate-fix commit regardless of its configured subject.

The CI step deliberately keeps watching the PR after checks pass, so
`axi run` returns `checks-passed` the moment checks are green (or a trusted
`no_ci: true` declaration covers a zero-check repository) rather than
blocking on the human merge. Never poll or re-run waiting for the merge yourself.
That monitor is live by default; only the opt-in
`ci.release_after_checks_pass` finishes the run at the verdict instead.
Never treat "no CI checks reported" alone as green.

Because that monitor stays live by default, a PR that falls behind the default branch or
hits a merge conflict after checks pass - commonly because another PR merged
first - needs **no command from you**: never hand-rebase. When the CI monitor
sees an actual conflict it **rebases onto the base, resolves it, revalidates from Review
because rebasing cannot prove continuity with the reviewed head, and re-pushes
the branch through Push**. On GitHub, a clean PR whose base requires branches
to be up to date (`mergeStateStatus: BEHIND`) still reports `checks-passed` and
waits for a human to bring it up to date; only when the repository opts
into `ci.rebase_behind` does the monitor give it the same rebase, once its
checks are green and the base has held still for one poll. A PR that is merely behind a base without that rule needs nothing, since
the platform merges it. The one exception is when that monitor is
no longer running - the PR was closed, the run was aborted or superseded, it
idle-timed-out, or its auto-fix attempts were exhausted - in which case recover
with `no-mistakes rerun`, subject to the clean-head check above. An accepted
rerun cancels the stale monitor and re-runs the full pipeline including a
deterministic rebase step. Do **not** reach for
`no-mistakes axi run` to refresh a still-active PR: with the default
monitor, after `checks-passed` it
reattaches to the running monitor (HEAD unchanged) and returns its output
without rebasing.

On a successful outcome (`checks-passed` or `passed`), close the loop with the
user: summarize what happened during the pipeline in a concise, easily readable
format - what was validated and what was found. If the output includes a
`fixes` table, the pipeline fixed findings your original change missed:
acknowledge those misses and explicitly list each fix so the user can easily
review them.

