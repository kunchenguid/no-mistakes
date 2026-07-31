---
name: no-mistakes
description: Validate code changes through the no-mistakes pipeline - code review, tests, lint, docs, push, PR, and CI - before they reach the push target. Use when the user asks to run no-mistakes, gate or validate changes before pushing, asks you to do a task and then validate it, or invokes /no-mistakes.
user-invocable: true
---

# no-mistakes

Drive `no-mistakes` through the `no-mistakes axi` command family: it prints
machine-readable [TOON](https://toonformat.dev) to stdout and progress to stderr.
The pipeline validates committed history through intent, rebase, review, test,
document, lint, push, PR, and CI before it reaches the configured push target.

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

## Validate and decide

Run the pipeline and decide on its findings as they come up. On a branch you
have not validated yet, clear [Before you start](#before-you-start) first.

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
   While a step is actively `running` or `fixing`, `axi status` may include
   `active_steps` with `active_for`, `last_activity`, a native `agent_pid` when
   a subprocess agent is running, and the current round such as `round 1`,
   `auto-fix 1/3`, or `fix 2`. If `last_activity` is prefixed with
   `quiet`, no step log or native-agent lifecycle activity has arrived for
   longer than `step_quiet_warning`. Treat that as a liveness clue, not as
   permission to cancel, rerun, or edit the worktree yourself.
2. If the output contains a `gate:` object, the pipeline is waiting on you.
   Read its `findings` table. Each finding has an `id`, `severity`,
   `file`, `description`, and an `action` that tells you how the
   pipeline classified it:
   - `auto-fix` - mechanical and low-risk; you can authorize the fix on
     your own judgment by responding with `--action fix`.
   - `no-op` - informational only; nothing to do.
   - `ask-user` - a call only the user can make; see
     [Escalate `ask-user` findings](#escalate-ask-user-findings).

   `severity` decides whether a finding is worth a fix round at all.
   Select `error` and `warning` findings; leave `info`
   findings out of `--findings` unless the user asked for them or one is
   plainly a real defect the reviewer under-rated. `info` is advisory - a
   preference, a nit, a possible future cleanup - and every finding you select
   costs a fix round plus the full rereview that round triggers. Reporting an
   `info` finding is the whole point of it; fixing it usually is not.
   The pipeline applies the same floor to its own automatic fixing
   (`auto_fix.min_severity`, default `warning`).

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
   The pipeline owns both the findings and the fixes: your job at a gate is to
   decide and respond, and `--action fix` has the pipeline apply the fix and
   re-review the result. Leave the worktree alone while a run is active - even
   for a real bug in your own code - because editing it yourself, or reaching
   for `abort` or `rerun` to do so, discards the pipeline's in-flight work
   and forces a full re-validation. `abort` and `rerun` are for *between*
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
   - `checks-passed` - the change is validated and CI is green (or the
     trusted default-branch config declares `no_ci: true` and no checks are
     registered - the help line names that declaration when it applies), but
     the PR is not merged yet. **You are done driving the pipeline.** Do not
     wait for the merge: tell the user the PR is ready and ask them to review
     and merge it (the PR link is in the `help` line). A generic empty forge
     check list without that declaration is not ready. no-mistakes keeps
     monitoring the PR in the background until it is merged, closed, or its
     configured idle timeout elapses, so a human can watch it in the TUI.
   - `passed` - the changes cleared the gate and the PR was merged or closed.
   - `failed` or `cancelled` - they did not; read the output and address it.
     Fix whatever the output points at (a failing test, a lint error, a finding
     you skipped), commit the fix on the same feature branch, then drive the
     pipeline again - `no-mistakes axi run --intent "..."` starts a fresh run,
     or `no-mistakes rerun` re-runs the pipeline for the current branch. Do
     not leave the user at a `failed` outcome without either retrying or
     explaining what blocks it.

The CI step deliberately keeps watching the PR after checks pass, so
`axi run` returns `checks-passed` the moment checks are green (or a trusted
`no_ci: true` declaration covers a zero-check repository) rather than
blocking on the human merge. Never poll or re-run waiting for the merge yourself.
Never treat "no CI checks reported" alone as green.

Because that monitor stays live, a PR that falls behind the default branch or
hits a merge conflict after checks pass - commonly because another PR merged
first - needs **no command from you**: leave it to the live monitor and
never hand-rebase it yourself. When the CI monitor sees an actual conflict it
**rebases onto the base, resolves it, and re-pushes the branch itself**; a PR
that is merely behind but still clean needs nothing either, since the platform
merges it. The one
exception is when that monitor is no longer running - the PR was closed, the run
was aborted or superseded, it idle-timed-out, or its auto-fix attempts were
exhausted - in which case recover with `no-mistakes rerun`, which cancels the
stale monitor and re-runs the full pipeline including a deterministic rebase
step. Reach for `no-mistakes axi run` only for a new run: on a still-active PR
it reattaches to the running monitor (HEAD unchanged) and returns its output
without rebasing.

Before any post-pipeline local commit or fresh run, read the structured
`branch_sync` object returned by AXI home, status, or a drive result and act
on its `next_action.code` as [sync-recovery.md](sync-recovery.md) describes.

On a successful outcome (`checks-passed` or `passed`), close the loop with the
user. If the output includes a `fixes` table, the pipeline fixed findings your
original change missed: acknowledge those misses and explicitly list each fix so
the user can easily review them.

## Two ways to invoke

When the user invokes `/no-mistakes`, report the outcome at the end. If the user
asks for something specific, translate that request into the matching `axi run`
flags yourself - for example, "skip the lint step" becomes `--skip=lint`. Run
`no-mistakes axi run --help` to see the available flags.

- **Validate-only** - bare `/no-mistakes` (optionally with flag-style requests
  like "skip the lint step"). The user's code changes are already committed;
  validate them and report the outcome.
- **Task-first** - `/no-mistakes <task>`, e.g.
  `/no-mistakes add a --json flag to the status command`. First carry out the
  task yourself, then validate the result through the pipeline:
  1. **Check scope.** Inspect `git status` before you change or commit anything.
     Preserve unrelated pre-existing uncommitted changes, and when you commit,
     commit only the changes that belong to the user's task.
  2. **Do the work.** Done means: every change the task requires is committed on
     a non-default branch, and `git status` shows only the unrelated
     pre-existing changes you found in step 1. If the user is on the
     repository's default branch, create a feature branch first - the gate
     validates committed history on a non-default branch, so the work must land
     there before you run.
  3. **Then validate**, passing the user's task as your `--intent`. The task
     text is exactly what the user set out to accomplish, in their own words, so
     it *is* the intent - preserve requirements stated directly by the user,
     including constraints, exclusions, acceptance criteria, and later decisions;
     do not condense them into a diff summary or drop them while adding
     implementation context. Enrich it with the decisions and tradeoffs you
     made while doing the work (see
     [Intent is required](#intent-is-required)).

## Test-quality rule

Never add a test whose only evidence is that it opens, reads, greps, parses, or
snapshots implementation source code and finds or omits particular strings,
tokens, lines, commands, function names, prompt phrases, regex matches, AST
shapes, or incidental snapshots. That does not prove behavior: matching text
can be dead or commented out, and a behavior-preserving refactor can change it.

Instead execute a public or executable interface and assert observable behavior,
state, output, side effects, and failure modes. For machine-consumed declarative
artifacts such as workflow YAML, JSON, policy, .gitignore, or generated
configuration, invoke the real consumer when feasible or parse into a typed or
normalized semantic model and assert meaning. A raw substring or regex over the
file is still the anti-pattern.

Reading a file is legitimate when the file is itself generated public output, a
serialized protocol, persisted state, an intentional snapshot, or another
explicitly owned text or byte contract. Name that contract, and do not use its
contents as a proxy that unrelated code works. A natural-language prompt or
instruction is not proven effective because its source contains a sentence.
Deterministic CI may test the final emitted prompt delivered to an agent as an
intentional generated interface; model interpretation belongs in
development-only evaluation, not live-LLM CI.

For a regression, reproduce the reported failure when feasible: the test should
fail before the fix and pass after it.

## Before you start

- The work you want validated must be **committed** on a branch. The gate
  validates committed history, not your uncommitted working tree.
- You must be on a **feature branch**, not the repository's default branch.
- The repository must already be initialized with `no-mistakes init`; run
  `no-mistakes init` if it is not.
- The daemon must have a runnable configured pipeline agent: a supported native
  agent binary, the `agent: cursor` ACP alias, or an explicit `acp:<target>` through
  `acpx`. You are the AXI driver, not
  an implicit pipeline-agent backend. If none is available, the run fails
  before its first step; `no-mistakes doctor` reports the configuration problem.

`axi run` fails closed on an unmet precondition and names the exact command to
fix it; run that command. If the `no-mistakes` command itself is missing or
misbehaving, `no-mistakes doctor` reports what is wrong.

Then run `no-mistakes axi` (home view).
If it shows an active run on your current branch, inspect it with `no-mistakes axi status`.
If it is parked at a gate, drive it with `no-mistakes axi respond`.
Reattach an in-flight run by re-running `no-mistakes axi run` when it still matches your current `HEAD` - either as the submitted head or as the current pipeline head.
Use `no-mistakes axi abort` only when you mean to discard that run before starting over.
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
no-mistakes axi status        # full detail plus cached branch_sync when relevant
no-mistakes axi logs --step <name> --full   # full log output of one step
no-mistakes axi abort         # cancel the current-branch active run
no-mistakes axi abort --run <id>   # cancel a specific run by id (works outside its worktree)
```

Exit codes: `0` success, no-op, or normal decision gates, `1` failed or cancelled final outcomes, `2` bad usage.

To parse an unfamiliar TOON shape - a gate block, a findings table, a final
state - read [reading-output.md](reading-output.md).
To return branch custody, synchronize, or recover unpublished pipeline commits,
read [sync-recovery.md](sync-recovery.md).
