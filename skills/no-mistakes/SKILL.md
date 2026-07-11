---
name: no-mistakes
description: Validate your code changes through the no-mistakes pipeline - automated code review, tests, lint, docs, push, PR, and CI - before they reach the configured push target. Use when the user asks to run no-mistakes, gate or ship or validate their changes, push safely, asks you to do a task and then validate it, or invokes /no-mistakes.
user-invocable: true
---

# no-mistakes

`no-mistakes` is a local gate that validates your code changes through a pipeline
(intent, rebase, review, test, document, lint, verify, push, PR, CI) before they reach
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
- The daemon must have a runnable configured pipeline agent: a supported native
  agent binary, or `acpx` for an `acp:<target>`. You are the AXI driver, not
  an implicit pipeline-agent backend. If none is available, the run fails
  before its first step; `no-mistakes doctor` reports the configuration problem.

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
     your own judgment by responding with `--action fix`, which sends it
     into the pipeline's routed repair cascade.
   - `no-op` - informational only; it is never repaired, even when selected.
   - `ask-user` - the finding challenges the user's deliberate intent or
     touches product behavior. This is a call only the user can make - see
     [Escalate `ask-user` findings](#escalate-ask-user-findings) below.

   **Blocking and ask-user review findings park for your decision** rather than
   being silently self-fixed. (Other steps such as test and lint may auto-fix
   within the pipeline and re-run before they ever gate.)

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
   re-review the result. A fix applies only to the finding IDs you select;
   findings you leave unselected park again for a decision at the next gate.
   For the same reason, while a run is active do **not**
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
   - `failed` or `cancelled` - they did not. Address the reported problem
     (a failing test, a lint error, a finding you skipped), commit the
     correction on the same feature branch, then start a fresh validation with
     `no-mistakes axi run --intent "..."` or use the TUI rerun action.
     Never abort or rerun an active run to bypass a gate - a fresh run or rerun
     is a *between-runs* action, correct only after a terminal outcome like
     this. `no-mistakes axi abort` is idempotent: no active run is a
     successful no-op. Do not leave the user at a `failed` outcome without
     either retrying or explaining what blocks it.

The CI step deliberately keeps watching the PR after checks pass, so
`axi run` returns `checks-passed` the moment checks are green rather than
blocking on the human merge. Never poll or re-run waiting for the merge yourself.

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

The one exception is `--yes` (below): it is the user's explicit standing
consent to drive every gate unattended, so under `--yes` you resolve
`ask-user` findings automatically instead of stopping to ask.

If you have clear consent to drive the run automatically, pass `--yes` to
`axi run` or `axi respond`. At an awaiting-approval gate it selects every
finding that has an ID for one pipeline fix round when any finding is
actionable, approves a gate with only `no-op` findings or no selectable
findings, and approves the resulting fix review rather than starting another
fix cycle. Before every unattended resolution, no-mistakes checks the run's
blocking repair lineages: if any blocking lineage ended its routed repair
cascade unresolved or inconclusive, unattended driving sends `abort` and
returns an error - it does not approve or retry the gate. `--yes` still
stops at `checks-passed`, because a human must review and merge the PR.
Only use `--yes` when the user has asked you to drive the whole run
without checking back.

Routing repair records each blocking finding as a lineage. A lineage that
finishes its finite repair cascade unresolved or inconclusive is not consented
away: unattended driving aborts, and a normal gate stays parked for an
explicit user decision.

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

A `gate:` waiting on you looks like this - a `gate:` object whose `step` names the awaiting step, optional step-specific fields such as `summary` and `note`, a `findings[N]{...}:` table with one row per finding, and an inline `help[N]:` list of next commands:

```
gate:
  step: review
  status: awaiting_approval
  summary: 1 ask-user decision and 1 mechanical fix
  note: Review findings are parked for explicit consent. ask-user requires the user's decision unless --yes supplies standing consent; --yes aborts rather than accepts an unresolved blocking repair.
  findings[2]{id,severity,file,action,description}:
    r1,warning,internal/pipeline/executor.go,auto-fix,Error from os.Remove is ignored
    r2,error,cmd/no-mistakes/main.go,ask-user,New --force flag bypasses the confirm prompt
help[5]: Run `no-mistakes axi respond --action approve` to accept this step and continue,Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself),Run `no-mistakes axi respond --action skip` to skip this step,Run `no-mistakes axi logs --step review --full` to read the full step log,"A long-running call is working, not stalled - background it if your harness needs to, but the run never advances past a gate on its own. Read every return; on a `gate:`, respond; loop until an `outcome:`."
```

Read the `action` column per row: decide `r1` (auto-fix) on your own
judgment - `respond --action fix --findings r1` hands it to the pipeline to
fix - but stop and escalate `r2` (ask-user) to the user before responding. A
final state
instead shows `outcome: <checks-passed|passed|failed|cancelled>` with no
`findings` table. Field names and exact columns can vary by step and version,
so read the actual `findings` header rather than assuming this layout.
