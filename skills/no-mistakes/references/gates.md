
## Validate and decide

Run the pipeline and decide on its findings as they come up:

1. Start the run. It blocks until the first decision point or the end:
   ```sh
   no-mistakes axi run --intent "<what the user set out to accomplish>"
   ```
   `axi run` and every `axi respond` block synchronously - the review, test,
   and CI steps can each take **several minutes**, so a single call may not
   return for a while. That is normal; do not cancel or re-issue the command
   because it seems slow. Both commands default to `--wait 8m` so a harness
   with a 10-minute tool cap gets a structured return instead of an unbounded
   hang. If the command returns because that wait elapsed, it is not a failed
   run and does not mean the daemon is dead: inspect with `no-mistakes axi status`
   and re-run `axi run` or `axi respond` to reattach. A slow live daemon is
   retried after a health probe rather than treated as I/O failure. To check
   progress without disturbing the run, use `no-mistakes axi status` from a
   separate call.
   A long-running call is working, not stalled - background it if your harness
   needs to, but the run **never advances past a gate on its own**. Read every
   return; on a `gate:`, respond; loop until an `outcome:`. Never idle-wait
   for the run to move forward by itself.
   When that status output includes `awaiting_agent: parked <duration>` under the run,
   the run is parked at an approval or fix-review gate and waiting for you to
   send `axi respond`. The field is observability only: it does not change
   gate resolution, auto-resume the run, or make `--yes` the default.
   While a step is actively `running` or `fixing`, `axi status` may include
   `active_steps` with step-scoped `active_for`, current-round `round_active_for`,
   `last_activity`, a native `agent_pid` when a subprocess agent is running, and the current round such as `round 1`,
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

   # fix some findings and explicitly decline the rest
   no-mistakes axi respond --action fix --findings <id1,id2> --ignore <id3>

   # skip this step
   no-mistakes axi respond --action skip

   # continue static review when coverage is incomplete and findings are empty
   no-mistakes axi respond --step review --action complete-coverage

   # reject this gate (only when the user has decided it must fail)
   no-mistakes axi respond --action abort --reason "<why the gate is rejected>"
   ```
   `complete-coverage` runs a fresh reviewer over only the missing paths.
   It reuses stored coverage only for the same run, step, round, head, base, and trusted scope.
   Missing or invalid provenance keeps Review gated. Source changes during the turn also keep it gated.
   It never approves incomplete coverage, runs a code fixer, or answers a question.
   It refuses outstanding findings, finding overrides, and `--yes`.
   With `--action fix`, declines are explicit: every finding the gate shows
   must be listed in `--findings` or `--ignore`, and a response that
   leaves one out is refused with the missing ids named while the gate stays
   parked. A finding an earlier round of this step already decided may be
   omitted to keep that decision - omitting it does **not** decline it - and a
   finding that round chose to fix cannot be declined by a later response at
   all: reverting an applied fix is out of scope for a gate response, so list
   it in `--findings` to fix it again or leave it out to keep it. Every
   successful `fix` response echoes what it recorded in a `recorded:` object
   (`fixed`, `ignored`, `kept`), so read it to confirm the decision
   you actually made.
   While a run is active, never fix findings by editing the code yourself -
   the pipeline owns both the findings and the fixes. Your job at a gate is to
   decide and respond; `--action fix` has the pipeline apply the fix and
   re-review the result. For the same reason, while a run is active do **not**
   `abort` or `rerun` to go fix a finding yourself - even a real bug in
   your own code - because that discards the pipeline's in-flight work and
   forces a full re-validation. `abort` and `rerun` are for *between*
   runs (after a `failed` or `cancelled` outcome), never to circumvent a
   gate.

    Each `respond` blocks until the next `gate:`, `checks-passed` decision point, or final outcome, subject to the same default `--wait 8m` hold.

    A review gate whose findings are `question-<id>` rows is waiting on answers to its reviewer's questions, not on a verdict: answer each with `no-mistakes axi answer --question <id> --answer "<one of its options>"` instead of approving or fixing. The answer that closes the last open question blocks exactly like `respond` and returns the next `gate:` or outcome; any other answer returns at once.

    Extra flags on `respond`:
    - `--wait` bounds the hold (default 8m).
    - `--reason "the operator's explanation"` records an explicitly authorized Test exception with `--step test --action approve`.
      This does not grant approval authority; escalate ask-user findings as before.
      Without a reason, Test approval remains effective; an approval past a failing command, `no-go`, or `inconclusive` verdict is reported as an exception with no operator reason supplied.
    - `--action abort --reason "<why>"` records the user's decision that the parked gate fails. The reason is required.
      The round is recorded as declined, no fix round starts, nothing is approved, pushed or merged, and the run ends `failed` with the reason in the step and run errors. Saved commits, findings and evidence stay available, but the run's worktree is removed, so uncommitted edits in it are lost.
      Follow the output's `branch_sync.next_action` to get the branch back. It is the user's call, never yours: do not send it to get past a finding, and it is not `axi abort`, which cancels the run instead.
    - Prefer `--instructions-file PATH` and `--add-finding-file PATH` for text containing backticks, quotes, or newlines. Write the exact text to a file and pass its path, without shell substitution. Alternatively `--instructions -` or `--add-finding -` reads stdin until EOF, cancellation, or `--wait` expiry (only one stdin input per response). Each file flag is exclusive with its inline flag; empty or whitespace-only file/stdin input is rejected.
    - `--add-finding '<json>'` (with `--action fix`) folds a finding you
      spotted yourself - one the pipeline did not surface - into the fix round,
      as a JSON finding object. Use it for a problem you noticed that is not in
      the gate's own `findings` table.
    - `--ignore <id1,id2>` (with `--action fix`) declines the named findings.
      Every finding in the gate's `findings` table must appear in
      `--findings` or `--ignore`; anything left out is refused, naming the
      ids, so an omission can never be recorded as a decline. A finding an
      earlier round of this step already chose to fix cannot be listed here:
      reverting an applied fix is out of scope for a gate response.
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
     Under the opt-in `ci.release_after_checks_pass` the run instead finishes at
     the verdict: `run.status` is `checks_passed`, the run released its worktree,
     and nothing is left watching the PR.
     An update it needs later is a fresh `no-mistakes axi run` on the updated head;
     this run will not rebase or re-push it.
   - `passed` - the pipeline completed under the requested steps, including any
     explicit per-run skips. This alone is not evidence that a PR was merged.
   - `passed-with-override` - the pipeline completed with an explicitly approved Test exception or CI failure.
     Report the exception, not a clean pass.
     Test evidence is in `run.test_override_reason`, including when CI readiness returns `checks-passed`; do not omit it from the summary.
   - `passed-with-skips` - publication or CI verification automatically skipped.
     Report the missing evidence and its cause from `run.automatic_skips`,
     bound to the full `run.head_sha`. This is neither CI readiness nor a
     failing code verdict. Explicit per-run skips retain their existing behavior.
   - `failed` or `cancelled` - they did not; read the output and address it.
     Follow the [custody guidance](recovery.md) before fixing whatever the output
     points at (a failing test, a lint error, a finding you skipped). Commit the
     fix on the same feature branch, then submit it with
     `no-mistakes axi run --intent "..."`. A fresh run or `rerun` is a
     *between-runs* action, correct only after a terminal outcome like this -
     never mid-run to circumvent a gate. Do not leave the user at a `failed`
     outcome without either retrying or explaining what blocks it.


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

The exception is `--yes` (below): it is the user's standing consent to
drive eligible gates unattended, so under `--yes` you resolve ordinary
`ask-user` findings automatically instead of stopping to ask.

If you have clear consent to drive the run automatically, pass `--yes` to `axi run`
or `axi respond`. For eligible gates, it treats actionable findings - `auto-fix` and
`ask-user` alike - as consent to fix it, selects every current finding for one
fix round, accepts the resulting fix review, and approves gates with only
`no-op` findings. Only use it when the user has asked you to drive the whole
run without checking back.

A `protected-path-refusal` gate still requires an explicit operator response
under `--yes`. Relay its path and rule; do not automatically fix, approve,
or skip it. Approval is rejected. Have the operator inspect and resolve the
reported edit, then send `--action fix` to retry the unfinished step.
The [protected-path reference](https://kunchenguid.github.io/no-mistakes/reference/repo-config/#protected_paths)
owns the staging guard's scope and limitations.

A gate holding open `question-<id>` findings is not eligible either: a question
is settled by `axi answer`, never by a verdict, so `--yes` stands aside
there - as it does when the question history cannot be read end to end, which
also parks for a human.

A `test-agent-unvalidated-work` finding means a timed-out Test agent left
commits or changes no Test turn validated. Approval is rejected, so `--yes`
stops at that gate without responding. Relay what the finding names and do
not skip Test, which would publish that work. Ask the operator to choose:
`--action fix` spends another agent budget to validate the work, and
`no-mistakes axi abort` stops the run.

