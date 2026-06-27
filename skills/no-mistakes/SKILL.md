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

When the user invokes `/no-mistakes`, report the outcome at the end. If the user
asks for something specific, translate that request into the matching `axi run`
flags yourself - for example, "skip the lint step" becomes `--skip=lint`. Run
`no-mistakes axi run --help` to see the available flags.

## Two ways to invoke

`/no-mistakes` works in two modes, depending on whether the user hands you a
task along with the command:

- **Validate-only** - bare `/no-mistakes` (optionally with flag-style requests
  like "skip the lint step"). The user's code changes are already committed;
  validate them and report the outcome.
- **Task-first** - `/no-mistakes <task>`, e.g.
  `/no-mistakes add a --json flag to the status command`. First carry out the
  task yourself, then validate the result through the pipeline:
  1. **Check scope.** Inspect `git status` before you change or commit anything.
     Preserve unrelated pre-existing uncommitted changes, and when you commit,
     commit only the changes that belong to the user's task.
  2. **Do the work.** Make the changes the task describes, then **commit them on
     a feature branch**. If the user is on the repository's default branch,
     create a feature branch first - the gate validates committed history on a
     non-default branch, so the work must land there before you run.
  3. **Then validate**, passing the user's task as your `--intent`. The task
     text is exactly what the user set out to accomplish, in their own words, so
     it *is* the intent - pass it through, enriched with the decisions and
     tradeoffs you made while doing the work (see
     [Intent is required](#intent-is-required)).

Everything below - preconditions, intent, the validate-and-decide loop - applies
the same way once the work is committed on a feature branch.

## Before you start

- The work you want validated must be **committed** on a branch. The gate
  validates committed history, not your uncommitted working tree.
- You must be on a **feature branch**, not the repository's default branch.
- The repository must already be initialized with `no-mistakes init`.

If any of these is not met, `axi run` returns an `error:` with the exact command
to fix it - read it and act on it (commit your work, or create a branch). If the
repository is not initialized, run `no-mistakes init` first; if the `no-mistakes`
command itself is missing or misbehaving, `no-mistakes doctor` reports what is
wrong.
Before starting, run `no-mistakes axi` (home view).
If it shows an active run on your current branch, inspect it with `no-mistakes axi status`.
If it is parked at a gate, drive it with `no-mistakes axi respond`.
Reattach an in-flight run by re-running `no-mistakes axi run` when it still matches your current `HEAD`.
Only `no-mistakes axi abort` it when you mean to discard that run before starting over; aborting is a between-runs action, never a way to take over or bypass a gate while a run is still going (see [Validate and decide](#validate-and-decide)).
If it shows an active run on another branch, leave that run alone and start validation for your current branch with `no-mistakes axi run --intent "..."`.

## Intent is required

When you start a run you must pass `--intent`: **what the user set out to
accomplish** - the goal or request behind this work, in their terms. This is not
a description of the diff or the files you changed; it is the objective the
change is meant to achieve. You know it from the conversation, so pass it
directly - no-mistakes uses it verbatim instead of inferring it from local agent
transcripts (slower and flakier).

Err on the side of completeness, not brevity. The review step uses `--intent`
to tell a deliberate decision apart from a mistake, so a thin one-line summary
makes it flag things the user already chose. Capture the nuance: the user's
goal, the specific decisions and tradeoffs they made along the way, any
constraints or approaches they ruled in or out, and anything they explicitly
asked for that might otherwise look surprising in the diff. A few sentences to a
short paragraph is normal - write down what you learned from the conversation
that a reviewer reading only the diff would not know.

## Validate and decide

Run the pipeline and decide on its findings as they come up:

1. Start the run. It blocks until the first decision point or the end:
   ```sh
   no-mistakes axi run --intent "<what the user set out to accomplish>"
   ```
   `axi run` and every `axi respond` block synchronously - the review, test,
   and CI steps can each take **several minutes**, so a single call may not
   return for a while. That is normal; allow a long timeout and do not cancel
   or re-issue the command because it seems slow. To check progress without
   disturbing the run, use `no-mistakes axi status` from a separate call.
   A long-running call is working, not stalled - background it if your harness
   needs to, but the run **never advances past a gate on its own**. Read every
   return; on a `gate:`, respond; loop until an `outcome:`. Never idle-wait
   for the run to move forward by itself.
   When that status output includes `awaiting_agent: parked <duration>` under the run,
   the run is parked at an approval or fix-review gate and waiting for you to
   send `axi respond`. The field is observability only: it does not change
   gate resolution, auto-resume the run, or make `--yes` the default.
2. If the output contains a `gate:` object, the pipeline is waiting on you.
   Read its `findings` table. Each finding has an `id`, `severity`,
   `file`, `description`, and an `action` that tells you how the
   pipeline classified it:
   - `auto-fix` - mechanical and low-risk; you can authorize the fix on
     your own judgment by responding with `--action fix`.
   - `no-op` - informational only; nothing to do.
   - `ask-user` - the finding challenges the user's deliberate intent or
     touches product behavior. This is a call only the user can make - see
     [Escalate `ask-user` findings](#escalate-ask-user-findings) below.

   **Review auto-fix is disabled by default** (`auto_fix.review: 0`; a repo
   or global `auto_fix.review > 0` override re-enables it), so blocking and
   ask-user review findings park for your decision rather than being silently
   self-fixed. (Other steps such as test and lint may auto-fix within the
   pipeline and re-run before they ever gate.)

   Choose one response:
   ```sh
   # accept the step as-is and continue
   no-mistakes axi respond --action approve

   # have the pipeline fix specific findings, then continue
   no-mistakes axi respond --action fix --findings <id1,id2> --instructions "<optional guidance>"

   # skip this step
   no-mistakes axi respond --action skip
   ```
   While a run is active, never fix findings by editing the code yourself -
   the pipeline owns both the findings and the fixes. Your job at a gate is to
   decide and respond; `--action fix` has the pipeline apply the fix and
   re-review the result. For the same reason, while a run is active do **not**
   `abort` or `rerun` to go fix a finding yourself - even a real bug in
   your own code - because that discards the pipeline's in-flight work and
   forces a full re-validation. `abort` and `rerun` are for *between*
   runs (after a `failed` or `cancelled` outcome), never to circumvent a
   gate.

    Each `respond` blocks until the next `gate:`, `checks-passed` decision point, or final outcome.

    Two extra flags are available on `respond` when you need them:
    - `--add-finding '<json>'` (with `--action fix`) folds a finding you
      spotted yourself - one the pipeline did not surface - into the fix round,
      as a JSON finding object. Use it for a problem you noticed that is not in
      the gate's own `findings` table.
    - `--step <name>` responds to a specific step instead of the one currently
      awaiting approval. You rarely need this; omit it to answer the active gate.
3. Repeat step 2 until the output has an `outcome:` instead of a `gate:`. The
   outcomes are:
   - `checks-passed` - the change is validated and CI is green, but the PR is
     not merged yet. **You are done driving the pipeline.** Do not wait for the
     merge: tell the user the PR is ready and ask them to review and merge it
     (the PR link is in the `help` line). no-mistakes keeps monitoring the PR
     in the background until it is merged, closed, or its configured idle
     timeout elapses, so a human can watch it in the TUI.
   - `passed` - the changes cleared the gate and the PR was merged or closed.
   - `failed` or `cancelled` - they did not; read the output and address it.
     Fix whatever the output points at (a failing test, a lint error, a finding
     you skipped), commit the fix on the same feature branch, then drive the
     pipeline again - `no-mistakes axi run --intent "..."` starts a fresh run,
     or `no-mistakes rerun` re-runs the pipeline for the current branch. This
     is the right place to start over: a fresh run or `rerun` is a
     *between-runs* action, correct only after a terminal outcome like this -
     never mid-run to circumvent a gate. Do not leave the user at a `failed`
     outcome without either retrying or explaining what blocks it.

The CI step deliberately keeps watching the PR after checks pass, so
`axi run` returns `checks-passed` the moment checks are green rather than
blocking on the human merge. Never poll or re-run waiting for the merge yourself.

Because that monitor stays live, a PR that falls behind the default branch or
hits a merge conflict after checks pass - commonly because another PR merged
first - needs **no command from you**: never hand-rebase. When the CI monitor
sees an actual conflict it **rebases onto the base, resolves it, and re-pushes
the branch itself**; a PR that is merely behind but still clean needs nothing
either, since the platform merges it. The one exception is when that monitor is
no longer running - the PR was closed, the run was aborted or superseded, it
idle-timed-out, or its auto-fix attempts were exhausted - in which case recover
with `no-mistakes rerun`, which cancels the stale monitor and re-runs the full
pipeline including a deterministic rebase step. Do **not** reach for
`no-mistakes axi run` to refresh a still-active PR: after `checks-passed` it
reattaches to the running monitor (HEAD unchanged) and returns its output
without rebasing.

On a successful outcome (`checks-passed` or `passed`), close the loop with the
user: summarize what happened during the pipeline in a concise, easily readable
format - what was validated and what was found. If the output includes a
`fixes` table, the pipeline fixed findings your original change missed:
acknowledge those misses and explicitly list each fix so the user can easily
review them.

## Escalate `ask-user` findings

A gate whose findings are all `auto-fix` or `no-op` is safe to drive on your
own judgment: respond with `--action fix` or `--action approve` as
appropriate. But a finding marked
`ask-user` is a decision that belongs to the user, not you - the pipeline
flagged it because it challenges their deliberate intent or changes product
behavior. Do not approve, fix, or skip it on your own. Instead, stop and bring
it to the user before you respond:

- Relay each `ask-user` finding to them as the pipeline wrote it - its
  `id`, `file`, and full `description` verbatim. Do not paraphrase,
  summarize away the detail, or pre-judge the answer.
- Ask how they want to proceed, then translate their decision into the matching
  `respond` call: `--action fix` (pass their guidance through
  `--instructions`), `--action approve`, or `--action skip`.

The one exception is `--yes` (below): it is the user's standing consent to
drive every gate unattended, so under `--yes` you resolve `ask-user`
findings automatically instead of stopping to ask.

If you have clear consent to drive the run automatically, pass `--yes` to `axi run`
or `axi respond`. It treats every actionable finding - `auto-fix` and
`ask-user` alike - as consent to fix it, selects every current finding for one
fix round, accepts the resulting fix review, and approves gates with only
`no-op` findings. Only use it when the user has asked you to drive the whole
run without checking back.

## Inspecting state

```sh
no-mistakes axi               # home view: current branch, active runs, next steps
no-mistakes axi status        # full detail of the resolved run
no-mistakes axi logs --step <name> --full   # full log output of one step
no-mistakes axi abort         # cancel the current-branch active run
no-mistakes axi abort --run <id>   # cancel a specific run by id (works outside its worktree)
```

## Reading the output

- Output is TOON: `key: value` pairs, `name[N]{cols}:` tables, and `help[N]:` hints.
- A non-terminal run object may include `awaiting_agent: parked <duration>` immediately after `status`; that means the run is parked at a gate awaiting your `axi respond`.
- The `help` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as `error: ...` on stdout with a `help` list; act on the suggestion.
- Exit codes: `0` success, no-op, or normal decision gates, `1` failed or cancelled final outcomes, `2` bad usage.

A `gate:` waiting on you looks roughly like this - a `gate:` line naming the step, optional step-specific fields such as `note`, a `findings[N]{...}:` table with one row per finding, and a `help[N]:` list of next commands:

```
gate: review
note: Review auto-fix is disabled by default (auto_fix.review: 0; a repo or global auto_fix.review > 0 override re-enables it), so blocking and ask-user review findings park for your decision rather than being silently self-fixed.
findings[2]{id,severity,file,line,action,description}:
  r1,warning,internal/pipeline/executor.go,,auto-fix,Error from os.Remove is ignored
  r2,error,cmd/no-mistakes/main.go,,ask-user,New --force flag bypasses the confirm prompt
help[3]:
  no-mistakes axi respond --action fix --findings r1
  no-mistakes axi respond --action approve
  A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a gate, respond; loop until an outcome.
```

Read the `action` column per row: decide `r1` (auto-fix) on your own
judgment - `respond --action fix --findings r1` hands it to the pipeline to
fix - but stop and escalate `r2` (ask-user) to the user before responding. A
final state
instead shows `outcome: <checks-passed|passed|failed|cancelled>` with no
`findings` table. Field names and exact columns can vary by step and version,
so read the actual `findings` header rather than assuming this layout.
