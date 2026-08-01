// Package skill holds the canonical content of the no-mistakes agent skill.
//
// It is the single source of truth for the skill's identity (name and
// trigger description) and every file the skill ships: SKILL.md plus the
// reference files it discloses. The genskill tool renders Files() to the
// public skills/no-mistakes/ directory (verified fresh in CI), and the init
// command installs the same files into the user-level agent skill directories
// under the user's home.
// The CLI's axi home view reuses Description so the two never drift.
package skill

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/gateguidance"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
)

// Name is the skill directory name and frontmatter name. It must match the
// installed directory so the agent exposes it as the /no-mistakes command.
const Name = "no-mistakes"

// Description is the trigger-shaped frontmatter description: what the skill
// does and when to use it. It is the single most important field for the
// agent's decision to load the skill, so it leads with outcomes and keywords.
// One trigger per distinct branch: validate already-committed work, or do a
// task and then validate it.
const Description = "Validate code changes through the no-mistakes pipeline - code review, tests, lint, docs, push, PR, and CI - before they reach the push target. Use when the user asks to run no-mistakes, gate or validate changes before pushing, asks you to do a task and then validate it, or invokes /no-mistakes."

// ReadingOutputFile and SyncRecoveryFile are the disclosed reference files
// SKILL.md points at by relative path. They are reached only on the branches
// that need them: parsing an unfamiliar TOON shape, and returning branch
// custody after a run.
const (
	ReadingOutputFile = "reading-output.md"
	SyncRecoveryFile  = "sync-recovery.md"
	SkillFile         = "SKILL.md"
)

// File is one file of the installed skill, named relative to the skill
// directory.
type File struct {
	Name    string
	Content string
}

// Files returns every file the skill ships, SKILL.md first. Install and
// genskill both render exactly this set, so the committed public skill and
// the user-level installation never drift.
func Files() []File {
	return []File{
		{Name: SkillFile, Content: Markdown()},
		{Name: ReadingOutputFile, Content: readingOutput},
		{Name: SyncRecoveryFile, Content: syncRecovery},
	}
}

// Bundle is every shipped file's content concatenated. It is the surface for
// guidance-sync checks: a canonical sentence must reach the agent somewhere in
// the skill, whether inline in SKILL.md or in a disclosed reference file.
func Bundle() string {
	var b strings.Builder
	for _, f := range Files() {
		b.WriteString(f.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// Markdown returns the complete SKILL.md document (YAML frontmatter plus body).
// The output is deterministic so it can be regenerated and diff-checked. It is
// the single rendering: the canonical public skill (surfaced by discovery
// tools, e.g. `npx skills add kunchenguid/no-mistakes`) and the copy init
// installs at user level are identical. Older versions vendored a variant with
// `metadata.internal: true` into each target repo to keep the vendored copy
// out of repo skill listings; the user-level install is a genuine user
// installation that should stay discoverable, so no internal marker exists
// anymore.
func Markdown() string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + Name + "\n")
	b.WriteString("description: " + Description + "\n")
	b.WriteString("user-invocable: true\n")
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

// body is the Markdown instructions an agent reads when the skill activates.
// Keep the top tier to what every invocation needs: the phase boundary, the
// validate-and-decide loop, then the material a single branch reaches.
// Reference that only some runs need belongs in a disclosed file (see Files).
// Do not embed live state here - the skill is static.
const body = `
# no-mistakes

Drive ` + "`no-mistakes`" + ` through the ` + "`no-mistakes axi`" + ` command family: it prints
machine-readable [TOON](https://toonformat.dev) to stdout and progress to stderr.
The pipeline validates committed history through intent, rebase, review, test,
document, lint, push, PR, and CI before it reaches the configured push target.
` + gateguidance.SkillBoundary + `
## Validate and decide

Run the pipeline and decide on its findings as they come up. On a branch you
have not validated yet, clear [Before you start](#before-you-start) first.

If the user asked you to *do* something rather than validate what is already
committed, carry out the task first and come back to this loop - see
[Two ways to invoke](#two-ways-to-invoke). Otherwise start the run now.

1. Start the run. It blocks until the first decision point or the end:
   ` + "```sh" + `
   no-mistakes axi run --intent "<what the user set out to accomplish>"
   ` + "```" + `
   ` + "`axi run`" + ` and every ` + "`axi respond`" + ` block synchronously - the review, test,
   and CI steps can each take **several minutes**, so a single call may not
   return for a while. That is normal; allow a long timeout and do not cancel
   or re-issue the command because it seems slow. To check progress without
   disturbing the run, use ` + "`no-mistakes axi status`" + ` from a separate call.
   A long-running call is working, not stalled - background it if your harness
   needs to, but the run **never advances past a gate on its own**. Read every
   return; on a ` + "`gate:`" + `, respond; loop until an ` + "`outcome:`" + `. Never idle-wait
   for the run to move forward by itself.
   That status output tells you whether the run is parked at a gate waiting on
   your ` + "`axi respond`" + ` and how long its active step has been working; the fields
   are described in [` + ReadingOutputFile + `](` + ReadingOutputFile + `).
2. If the output contains a ` + "`gate:`" + ` object, the pipeline is waiting on you.
   Read its ` + "`findings`" + ` table. Two fields drive your decision: ` + "`action`" + `,
   which tells you how the pipeline classified the finding, and ` + "`severity`" + `,
   which tells you whether it is worth a fix round. Select findings by their
   ` + "`id`" + `. The ` + "`action`" + ` values are:
   - ` + "`auto-fix`" + ` - mechanical and low-risk; you can authorize the fix on
     your own judgment by responding with ` + "`--action fix`" + `.
   - ` + "`no-op`" + ` - informational only; nothing to do.
   - ` + "`ask-user`" + ` - a call only the user can make; see
     [Escalate ` + "`ask-user`" + ` findings](#escalate-ask-user-findings).

   To parse an unfamiliar TOON shape - a gate block, a findings table, a final
   state - read [` + ReadingOutputFile + `](` + ReadingOutputFile + `).

   ` + "`severity`" + ` decides whether a finding is worth a fix round at all.
   Select ` + "`error`" + ` and ` + "`warning`" + ` findings; leave ` + "`info`" + `
   findings out of ` + "`--findings`" + ` unless the user asked for them or one is
   plainly a real defect the reviewer under-rated. ` + "`info`" + ` is advisory - a
   preference, a nit, a possible future cleanup - and every finding you select
   costs a fix round plus the full rereview that round triggers. Reporting an
   ` + "`info`" + ` finding is the whole point of it; fixing it usually is not.
   The pipeline applies the same floor to its own automatic fixing
   (` + "`auto_fix.min_severity`" + `, default ` + "`warning`" + `).

   **Review auto-fix is disabled by default** (` + "`auto_fix.review: 0`" + `; a repo
   or global ` + "`auto_fix.review > 0`" + ` override re-enables it), so blocking and
   ask-user review findings park for your decision rather than being silently
   self-fixed. (Other steps such as test and lint may auto-fix within the
   pipeline and re-run before they ever gate.)

   Choose one response:
   ` + "```sh" + `
   # accept the step as-is and continue
   no-mistakes axi respond --action approve

   # have the pipeline fix specific findings, then continue
   no-mistakes axi respond --action fix --findings <id1,id2> --instructions "<optional guidance>"

   # skip this step
   no-mistakes axi respond --action skip
   ` + "```" + `
   The pipeline owns both the findings and the fixes: your job at a gate is to
   decide and respond, and ` + "`--action fix`" + ` has the pipeline apply the fix and
   re-review the result. Leave the worktree alone while a run is active - even
   for a real bug in your own code - because editing it yourself, or reaching
   for ` + "`abort`" + ` or ` + "`rerun`" + ` to do so, discards the pipeline's in-flight work
   and forces a full re-validation. Never
   abort or rerun while a gate awaits your response or a step is actively
   working, unless you are deliberately discarding that run.

   Each ` + "`respond`" + ` blocks until the next ` + "`gate:`" + `, ` + "`checks-passed`" + ` decision point, or final outcome.

   Two extra flags are available on ` + "`respond`" + ` when you need them:
   - ` + "`--add-finding '<json>'`" + ` (with ` + "`--action fix`" + `) folds a finding you
     spotted yourself - one the pipeline did not surface - into the fix round,
     as a JSON finding object. Use it for a problem you noticed that is not in
     the gate's own ` + "`findings`" + ` table.
   - ` + "`--step <name>`" + ` responds to a specific step instead of the one currently
     awaiting approval. You rarely need this; omit it to answer the active gate.

   If the user asked you to drive the whole run without checking back, resolve
   every gate with ` + "`--yes`" + ` instead - see
   [Drive unattended with ` + "`--yes`" + `](#drive-unattended-with---yes).
3. Repeat step 2 until the output has an ` + "`outcome:`" + ` instead of a ` + "`gate:`" + `. The
   outcomes are:
   - ` + "`checks-passed`" + ` - the change is validated and CI is green (or the
     trusted default-branch config declares ` + "`no_ci: true`" + ` and no checks are
     registered - the help line names that declaration when it applies), but the
     PR is not merged yet. The CI step deliberately returns here the moment
     checks are green rather than blocking on the human merge, so **you are done
     driving the pipeline.** Do not wait, poll, or re-run for the merge: tell the
     user the PR is ready and ask them to review and merge it (the PR link is in
     the ` + "`help`" + ` line). A generic empty forge check list without that
     declaration is not ready - never treat "no CI checks reported" alone as
     green. no-mistakes keeps monitoring the PR in the background until it is
     merged, closed, or its configured idle timeout elapses, so a human can watch
     it in the TUI.
   - ` + "`passed`" + ` - the changes cleared the gate and the PR was merged or closed.
   - ` + "`failed`" + ` or ` + "`cancelled`" + ` - they did not; read the output and address it.
     Fix whatever the output points at (a failing test, a lint error, a finding
     you skipped), commit the fix on the same feature branch, then start a fresh
     run with ` + "`no-mistakes axi run --intent \"...\"`" + `, which validates the new
     local ` + "`HEAD`" + ` you just committed. Do not reach for
     ` + "`no-mistakes rerun`" + ` after a local commit:
     it re-validates the head already pushed to the gate, so it is
     only for an unchanged local HEAD (a dead or stale CI monitor) and here
     would silently re-check the pre-fix code. Do not leave the user at a
     ` + "`failed`" + ` outcome without either retrying or explaining what blocks it.

Because that background monitor stays live, a PR that falls behind the default branch or
hits a merge conflict after checks pass - commonly because another PR merged
first - needs **no command from you**: leave it to the live monitor and
never hand-rebase it yourself. When the CI monitor sees an actual conflict it
**rebases onto the base, resolves it, and re-pushes the branch itself**; a PR
that is merely behind but still clean needs nothing either, since the platform
merges it. The one
exception is when that monitor is no longer running - the PR was closed, the run
was aborted or superseded, it idle-timed-out, or its auto-fix attempts were
exhausted - in which case recover with ` + "`no-mistakes rerun`" + `, which cancels the
stale monitor and re-runs the full pipeline including a deterministic rebase
step. If the dead run left auto-fix or CI-rebase commits your clone lacks, take
them with the offered ` + "`branch_sync`" + ` ` + "`sync`" + ` action **before the rerun,
not after**: the rerun creates a pending run with no push binding
(` + "`legacy_unbound`" + `), and ` + "`no-mistakes axi sync`" + ` then refuses.
` + "`no-mistakes rerun`" + ` only *starts* that run:
it returns immediately without driving, so something still has to answer the
recovered run's gates.
Follow it with ` + "`no-mistakes axi run`" + `, then resume the
step-2 gate loop until you get an ` + "`outcome:`" + `. That reattach is conditional:
the reran run carries the head the **gate** holds, while ` + "`axi run`" + ` looks up an
active run by your **local** ` + "`HEAD`" + `, so it reattaches - with no ` + "`--intent`" + ` -
only while the gate head still equals your local HEAD, which is exactly what
syncing first establishes. Never point
` + "`no-mistakes axi run`" + ` at a **still-active** PR to refresh it: it reattaches to
the running monitor and returns its output without rebasing.

Before any post-pipeline local commit or fresh run, read the structured
` + "`branch_sync`" + ` object returned by AXI home, status, or a drive result and act
on its ` + "`next_action.code`" + ` as [` + SyncRecoveryFile + `](` + SyncRecoveryFile + `) describes.

On a successful outcome (` + "`checks-passed`" + ` or ` + "`passed`" + `), close the loop with the
user. If the output includes a ` + "`fixes`" + ` table, the pipeline fixed findings your
original change missed: acknowledge those misses and explicitly list each fix so
the user can easily review them.

## Two ways to invoke

When the user invokes ` + "`/no-mistakes`" + `, report the outcome at the end. If the user
asks for something specific, translate that request into the matching ` + "`axi run`" + `
flags yourself - for example, "skip the lint step" becomes ` + "`--skip=lint`" + `. Run
` + "`no-mistakes axi run --help`" + ` to see the available flags.

- **Validate-only** - bare ` + "`/no-mistakes`" + ` (optionally with flag-style requests
  like "skip the lint step"). The user's code changes are already committed;
  validate them and report the outcome.
- **Task-first** - ` + "`/no-mistakes <task>`" + `, e.g.
  ` + "`/no-mistakes add a --json flag to the status command`" + `. First carry out the
  task yourself, then validate the result through the pipeline:
  1. **Check scope.** Inspect ` + "`git status`" + ` before you change or commit anything.
     Preserve unrelated pre-existing uncommitted changes, and when you commit,
     commit only the changes that belong to the user's task.
  2. **Do the work.** Done means: every change the task requires is committed on
     a non-default branch, and ` + "`git status`" + ` shows only the unrelated
     pre-existing changes you found in step 1. If the user is on the
     repository's default branch, create a feature branch first - the gate
     validates committed history on a non-default branch, so the work must land
     there before you run.
  3. **Then validate**, passing the user's task as your ` + "`--intent`" + `. The task
     text is exactly what the user set out to accomplish, in their own words, so
     it *is* the intent - preserve requirements stated directly by the user,
     including constraints, exclusions, acceptance criteria, and later decisions;
     do not condense them into a diff summary or drop them while adding
     implementation context. Enrich it with the decisions and tradeoffs you
     made while doing the work (see
     [Intent is required](#intent-is-required)).
` + testguidance.Rule + `
## Before you start

- The work you want validated must be **committed** on a branch. The gate
  validates committed history, not your uncommitted working tree.
- You must be on a **feature branch**, not the repository's default branch.
- The repository must already be initialized with ` + "`no-mistakes init`" + `; run
  ` + "`no-mistakes init`" + ` if it is not.
- The daemon must have a runnable configured pipeline agent: a supported native
  agent binary, the ` + "`agent: cursor`" + ` ACP alias, or an explicit ` + "`acp:<target>`" + ` through
  ` + "`acpx`" + `. You are the AXI driver, not
  an implicit pipeline-agent backend. If none is available, the run fails
  before its first step; ` + "`no-mistakes doctor`" + ` reports the configuration problem.

` + "`axi run`" + ` fails closed on an unmet precondition and names the exact command to
fix it; run that command. If the ` + "`no-mistakes`" + ` command itself is missing or
misbehaving, ` + "`no-mistakes doctor`" + ` reports what is wrong.

Then run ` + "`no-mistakes axi`" + ` (home view).
If it shows an active run on your current branch, inspect it with ` + "`no-mistakes axi status`" + `.
If it is parked at a gate, drive it with ` + "`no-mistakes axi respond`" + `.
Reattach an in-flight run by re-running ` + "`no-mistakes axi run`" + ` when it still matches your current ` + "`HEAD`" + ` - either as the submitted head or as the current pipeline head.
Use ` + "`no-mistakes axi abort`" + ` only when you mean to discard that run before starting over.
If it shows an active run on another branch, leave that run alone and start validation for your current branch with ` + "`no-mistakes axi run --intent \"...\"`" + `.

## Intent is required

When you start a run you must pass ` + "`--intent`" + `: **what the user set out to
accomplish** - the goal or request behind this work, in their terms. This is not
a description of the diff or the files you changed; it is the objective the
change is meant to achieve. You know it from the conversation, so pass it
directly - no-mistakes uses it verbatim instead of inferring it from local agent
transcripts (slower and flakier).

Err on the side of completeness, not brevity. The review step uses ` + "`--intent`" + `
to tell a deliberate decision apart from a mistake, so a thin one-line summary
makes it flag things the user already chose. Capture the nuance: the user's
goal, the specific decisions and tradeoffs they made along the way, any
constraints or approaches they ruled in or out, and anything they explicitly
asked for that might otherwise look surprising in the diff. A few sentences to a
short paragraph is normal - write down what you learned from the conversation
that a reviewer reading only the diff would not know.

## Escalate ` + "`ask-user`" + ` findings

A finding marked
` + "`ask-user`" + ` is a decision that belongs to the user, not you - the pipeline
flagged it because it challenges their deliberate intent or changes product
behavior. Do not approve, fix, or skip it on your own. Instead, stop and bring
it to the user before you respond:

- Relay each ` + "`ask-user`" + ` finding to them as the pipeline wrote it - its
  ` + "`id`" + `, ` + "`file`" + `, and full ` + "`description`" + ` verbatim. Do not paraphrase,
  summarize away the detail, or pre-judge the answer.
- Ask how they want to proceed, then translate their decision into the matching
  ` + "`respond`" + ` call: ` + "`--action fix`" + ` (pass their guidance through
  ` + "`--instructions`" + `), ` + "`--action approve`" + `, or ` + "`--action skip`" + `.

The one exception is
[` + "`--yes`" + `](#drive-unattended-with---yes): it is the user's standing consent to
drive every gate unattended, so under ` + "`--yes`" + ` you resolve ` + "`ask-user`" + `
findings automatically instead of stopping to ask.

## Drive unattended with ` + "`--yes`" + `

If you have clear consent to drive the run automatically, pass ` + "`--yes`" + ` to ` + "`axi run`" + `
or ` + "`axi respond`" + `. It treats every actionable finding - ` + "`auto-fix`" + ` and
` + "`ask-user`" + ` alike - as consent to fix it, selects every current finding for one
fix round, accepts the resulting fix review, and approves gates with only
` + "`no-op`" + ` findings. Only use it when the user has asked you to drive the whole
run without checking back.

` + "`--yes`" + ` converges; it does not fix until clean. Each step is fixed at most
once, so if the rereview of that fix still reports blocking findings, ` + "`--yes`" + `
approves the gate and the run moves on rather than looping. A run driven this
way can therefore reach a successful outcome with findings still outstanding -
report them to the user.

## Inspecting state

` + "```sh" + `
no-mistakes axi               # home view: current branch, active runs, next steps
no-mistakes axi status        # full detail plus cached branch_sync when relevant
no-mistakes axi logs --step <name> --full   # full log output of one step
no-mistakes axi abort         # cancel the current-branch active run
no-mistakes axi abort --run <id>   # cancel a specific run by id (works outside its worktree)
` + "```" + `

Exit codes: ` + "`0`" + ` success, no-op, or normal decision gates; ` + "`1`" + ` either a ` + "`failed`" + `/` + "`cancelled`" + ` final outcome **or** an ` + "`error:`" + ` document (uninitialized repo, refused precondition, refused branch ownership, ` + "`nested_gate_context`" + `); ` + "`2`" + ` bad usage.
Exit ` + "`1`" + ` alone does not mean the run failed - read whether you got an ` + "`outcome:`" + ` or an ` + "`error:`" + ` before you tell the user anything.
`

// readingOutput is the disclosed TOON-format reference: reached when the agent
// meets an output shape it cannot read from the loop alone.
const readingOutput = `# Reading AXI output

- Output is TOON: ` + "`key: value`" + ` pairs, ` + "`name[N]{cols}:`" + ` tables, and ` + "`help[N]:`" + ` hints.
- The ` + "`help`" + ` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as ` + "`error: ...`" + ` on stdout with a ` + "`help`" + ` list; act on the suggestion.
- A final state shows ` + "`outcome: <checks-passed|passed|failed|cancelled>`" + ` with no ` + "`findings`" + ` table.
- Field names and exact columns vary by step and version, so read the actual ` + "`findings`" + ` header rather than assuming a layout.
- A successful outcome may carry a ` + "`fixes[N]{step,summary}:`" + ` table - one row per fix round the pipeline applied, in step then round order, where ` + "`summary`" + ` describes what that round changed (a round that recorded no summary shows ` + "`fix applied (no summary recorded)`" + `). Acknowledge those misses and list each fix for the user.

## A gate block

A ` + "`gate:`" + ` waiting on you looks roughly like this - a ` + "`gate:`" + ` line naming the step, optional step-specific fields such as ` + "`note`" + `, a ` + "`findings[N]{...}:`" + ` table with one row per finding, and a ` + "`help[N]:`" + ` list of next commands:

` + "```" + `
gate: review
note: Review auto-fix is disabled by default, so blocking and ask-user review findings park for your decision.
findings[2]{id,severity,file,line,action,description}:
  r1,warning,internal/pipeline/executor.go,,auto-fix,Error from os.Remove is ignored
  r2,error,cmd/no-mistakes/main.go,,ask-user,New --force flag bypasses the confirm prompt
help[4]:
  Run ` + "`no-mistakes axi respond --action approve`" + ` to accept this step and continue
  Run ` + "`no-mistakes axi respond --action fix --findings <ids>`" + ` to have the pipeline fix the selected findings (do not edit files yourself)
  Run ` + "`no-mistakes axi respond --action skip`" + ` to skip this step
  Run ` + "`no-mistakes axi logs --step review --full`" + ` to read the full step log
` + "```" + `

## Run-progress fields

` + "`axi status`" + ` and other non-terminal run objects report progress with these fields:

- ` + "`awaiting_agent: parked <duration>`" + `, immediately after ` + "`status`" + `, means the run is parked at an approval or fix-review gate and waiting for you to send ` + "`axi respond`" + `. It is observability only: it does not change gate resolution, auto-resume the run, or make ` + "`--yes`" + ` the default.
- ` + "`active_steps`" + ` appears while a step is ` + "`running`" + ` or ` + "`fixing`" + `, with ` + "`active_for`" + `, ` + "`last_activity`" + `, a native ` + "`agent_pid`" + ` when a subprocess agent is running, and the current round such as ` + "`round 1`" + `, ` + "`auto-fix 1/3`" + `, or ` + "`fix 2`" + `.
- A ` + "`last_activity`" + ` prefixed with ` + "`quiet`" + ` means no step log or native-agent lifecycle activity has arrived for longer than ` + "`step_quiet_warning`" + `. Treat that as a liveness clue only.
`

// syncRecovery is the disclosed branch-custody reference: reached before a
// post-pipeline local commit or a fresh run, and when a terminal run left
// pipeline commits unpublished.
const syncRecovery = `# Branch synchronization and custody recovery

` + "```sh" + `
no-mistakes axi sync --check    # freshly verify an offered synchronization plan
no-mistakes axi sync            # apply only an offered guarded synchronization
no-mistakes axi sync --recover  # return custody after a terminal run left unpublished pipeline commits
` + "```" + `

Every ` + "`next_action`" + ` carries both a ` + "`code`" + ` and the exact ` + "`command`" + ` to run, and an object with no ` + "`next_action`" + ` needs nothing from you (the branch is already synchronized, or the state is purely informational). Run the reported command for whatever code you get, including any code not listed below; the notes here only add what the command alone does not tell you:

- ` + "`sync`" + ` - the guarded sync may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics; genuine divergence stays blocked.
- ` + "`check_sync`" + ` and ` + "`retry`" + ` - the reported state is not trustworthy yet (the pipeline-pushed commit is missing locally, or the push target could not be refreshed). Run the reported check and re-read the result before deciding anything.
- ` + "`run_pipeline`" + ` - your local head is ahead of the pipeline-pushed head, so the way forward is a new run, not a synchronization.
- ` + "`continue_active_run`" + ` - the pipeline still owns the branch: keep driving the active run rather than making local follow-up commits.
- ` + "`recover_custody`" + ` - a terminal run left unpublished pipeline commits preserved in the local gate. Recover custody first with ` + "`no-mistakes axi sync --recover`" + `: it returns custody and moves a clean worktree to the preserved pipeline head, by fast-forward or by adopting a diverged preserved head proven to carry every local change - the ordinary result of the pipeline rebasing your commits onto a newer base - after anchoring your pre-recovery head under ` + "`refs/no-mistakes/recover-local/<run>`" + `. That proof is deliberately narrow, so a rebase whose fix rounds also rewrote your own lines refuses instead of being adopted: when nothing can tell a deliberate pipeline fix from a dropped change, the decision is yours. Then validate that head with ` + "`no-mistakes axi run --intent \"...\"`" + `, which starts and drives the run in one command. ` + "`no-mistakes rerun`" + ` also re-runs the preserved pipeline head, but it returns immediately without driving, and a following ` + "`no-mistakes axi run`" + ` reattaches only while your local HEAD equals that preserved head - so use it only after the recovery moved your worktree there. A dirty worktree, or divergence that cannot be proven contained, makes the recovery refuse with explicit choices; ` + "`--keep-local`" + ` keeps your current head while the preserved commits stay anchored under ` + "`refs/no-mistakes/recover/<run>`" + `.
- ` + "`inspect_worktree`" + ` and ` + "`inspect_and_reconcile_manually`" + ` - the operation refused and changed nothing. The reported command only shows you the situation; it does not resolve it.

A ` + "`branch_sync.state`" + ` of ` + "`user_owned`" + ` means the run went terminal before changing the submitted head and cancellation released the branch: the exact branch and head are yours and immediately usable for whichever delivery path is authorized - no sync action is needed, and a repeated ` + "`--recover`" + ` there is a harmless no-op.

When synchronization is blocked, relay that structured state and its offered choices to the user and act only on them, instead of improvising reset, stash, merge, rebase, force, or branch replacement.

After synchronization, commit the post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present, then re-run ` + "`no-mistakes axi run --intent \"...\"`" + ` with the original user intent.
`
