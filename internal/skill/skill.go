// Package skill holds the canonical content of the no-mistakes agent skill.
//
// It is the single source of truth for the skill's identity (name and
// trigger description) and its SKILL.md body. The genskill tool renders
// Markdown() to skills/no-mistakes/SKILL.md (verified fresh in CI), and the
// init command embeds that generated file to install it into a user's repo.
// The CLI's axi home view reuses Description so the two never drift.
package skill

import "strings"

// Name is the skill directory name and frontmatter name. It must match the
// installed directory so the agent exposes it as the /no-mistakes command.
const Name = "no-mistakes"

// Description is the trigger-shaped frontmatter description: what the skill
// does and when to use it. It is the single most important field for the
// agent's decision to load the skill, so it leads with outcomes and keywords.
const Description = "Validate your code changes through the no-mistakes pipeline - automated code review, tests, lint, docs, push, PR, and CI - before they reach upstream. Use when the user asks to run no-mistakes, gate or ship or validate their changes, push safely, asks you to do a task and then validate it, or invokes /no-mistakes."

// Markdown returns the complete SKILL.md document (YAML frontmatter plus body).
// The output is deterministic so it can be regenerated and diff-checked.
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
// Keep it focused: the operating loop, the command vocabulary, and how to read
// the TOON output. Do not embed live state here - the skill is static.
const body = `
# no-mistakes

` + "`no-mistakes`" + ` is a local gate that validates your code changes through a pipeline
(intent, rebase, review, test, document, lint, push, PR, CI) before they reach
upstream. You drive it through the ` + "`no-mistakes axi`" + ` command family, which prints
machine-readable [TOON](https://toonformat.dev) to stdout and progress to stderr.

When the user invokes ` + "`/no-mistakes`" + `, report the outcome at the end. If the user
asks for something specific, translate that request into the matching ` + "`axi run`" + `
flags yourself - for example, "skip the lint step" becomes ` + "`--skip=lint`" + `. Run
` + "`no-mistakes axi run --help`" + ` to see the available flags.

## Two ways to invoke

` + "`/no-mistakes`" + ` works in two modes, depending on whether the user hands you a
task along with the command:

- **Validate-only** - bare ` + "`/no-mistakes`" + ` (optionally with flag-style requests
  like "skip the lint step"). The user's code changes are already committed;
  validate them and report the outcome.
- **Task-first** - ` + "`/no-mistakes <task>`" + `, e.g.
  ` + "`/no-mistakes add a --json flag to the status command`" + `. First carry out the
  task yourself, then validate the result through the pipeline:
  1. **Check scope.** Inspect ` + "`git status`" + ` before you change or commit anything.
     Preserve unrelated pre-existing uncommitted changes, and when you commit,
     commit only the changes that belong to the user's task.
  2. **Do the work.** Make the changes the task describes, then **commit them on
     a feature branch**. If the user is on the repository's default branch,
     create a feature branch first - the gate validates committed history on a
     non-default branch, so the work must land there before you run.
  3. **Then validate**, passing the user's task as your ` + "`--intent`" + `. The task
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
- The repository must already be initialized with ` + "`no-mistakes init`" + `.

If any of these is not met, ` + "`axi run`" + ` returns an ` + "`error:`" + ` with the exact command
to fix it - read it and act on it (commit your work, or create a branch). If the
repository is not initialized, run ` + "`no-mistakes init`" + ` first; if the ` + "`no-mistakes`" + `
command itself is missing or misbehaving, ` + "`no-mistakes doctor`" + ` reports what is
wrong. Before starting, a quick ` + "`no-mistakes axi`" + ` (home view) shows whether a
run is already active - resume or ` + "`axi abort`" + ` it rather than starting a second
run on top of it.

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

## Validate and decide

Run the pipeline and decide on its findings as they come up:

1. Start the run. It blocks until the first decision point or the end:
   ` + "```sh" + `
   no-mistakes axi run --intent "<what the user set out to accomplish>"
   ` + "```" + `
   ` + "`axi run`" + ` and every ` + "`axi respond`" + ` block synchronously - the review, test,
   and CI steps can each take **several minutes**, so a single call may not
   return for a while. That is normal; allow a long timeout and do not cancel
   or re-issue the command because it seems slow. To check progress without
   disturbing the run, use ` + "`no-mistakes axi status`" + ` from a separate call.
2. If the output contains a ` + "`gate:`" + ` object, the pipeline is waiting on you.
   Read its ` + "`findings`" + ` table. Each finding has an ` + "`id`" + `, ` + "`severity`" + `,
   ` + "`file`" + `, ` + "`description`" + `, and an ` + "`action`" + ` that tells you how the
   pipeline classified it:
   - ` + "`auto-fix`" + ` - a mechanical, low-risk fix you can safely make yourself.
   - ` + "`no-op`" + ` - informational only; nothing to do.
   - ` + "`ask-user`" + ` - the finding challenges the user's deliberate intent or
     touches product behavior. This is a call only the user can make - see
     [Escalate ` + "`ask-user`" + ` findings](#escalate-ask-user-findings) below.

   Choose one response:
   ` + "```sh" + `
   # accept the step as-is and continue
   no-mistakes axi respond --action approve

   # have the agent fix specific findings, then continue
   no-mistakes axi respond --action fix --findings <id1,id2> --instructions "<optional guidance>"

   # skip this step
   no-mistakes axi respond --action skip
   ` + "```" + `
    Each ` + "`respond`" + ` blocks until the next ` + "`gate:`" + `, ` + "`checks-passed`" + ` decision point, or final outcome.

    Two extra flags are available on ` + "`respond`" + ` when you need them:
    - ` + "`--add-finding '<json>'`" + ` (with ` + "`--action fix`" + `) folds a finding you
      spotted yourself - one the pipeline did not surface - into the fix round,
      as a JSON finding object. Use it for a problem you noticed that is not in
      the gate's own ` + "`findings`" + ` table.
    - ` + "`--step <name>`" + ` responds to a specific step instead of the one currently
      awaiting approval. You rarely need this; omit it to answer the active gate.
3. Repeat step 2 until the output has an ` + "`outcome:`" + ` instead of a ` + "`gate:`" + `. The
   outcomes are:
   - ` + "`checks-passed`" + ` - the change is validated and CI is green, but the PR is
     not merged yet. **You are done driving the pipeline.** Do not wait for the
     merge: tell the user the PR is ready and ask them to review and merge it
     (the PR link is in the ` + "`help`" + ` line). no-mistakes keeps monitoring the PR
     in the background, so a human can watch it in the TUI.
   - ` + "`passed`" + ` - the changes cleared the gate and the PR was merged or closed.
   - ` + "`failed`" + ` or ` + "`cancelled`" + ` - they did not; read the output and address it.
     Fix whatever the output points at (a failing test, a lint error, a finding
     you skipped), commit the fix on the same feature branch, then drive the
     pipeline again - ` + "`no-mistakes axi run --intent \"...\"`" + ` starts a fresh run,
     or ` + "`no-mistakes rerun`" + ` re-runs the pipeline for the current branch. Do not
     leave the user at a ` + "`failed`" + ` outcome without either retrying or explaining
     what blocks it.

The CI step deliberately watches the PR until it is merged or closed, so
` + "`axi run`" + ` returns ` + "`checks-passed`" + ` the moment checks are green rather than
blocking on the human merge. Never poll or re-run waiting for the merge yourself.

## Escalate ` + "`ask-user`" + ` findings

A gate whose findings are all ` + "`auto-fix`" + ` or ` + "`no-op`" + ` is safe to drive on your
own judgment: fix or approve as appropriate. But a finding marked
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

The one exception is ` + "`--yes`" + ` (below): it is the user's standing consent to
drive every gate unattended, so under ` + "`--yes`" + ` you resolve ` + "`ask-user`" + `
findings automatically instead of stopping to ask.

If you have clear consent to drive the run automatically, pass ` + "`--yes`" + ` to ` + "`axi run`" + `
or ` + "`axi respond`" + `. It treats every actionable finding - ` + "`auto-fix`" + ` and
` + "`ask-user`" + ` alike - as consent to fix it, selects every current finding for one
fix round, accepts the resulting fix review, and approves gates with only
` + "`no-op`" + ` findings. Only use it when the user has asked you to drive the whole
run without checking back.

## Inspecting state

` + "```sh" + `
no-mistakes axi               # home view: active run, recent runs, next steps
no-mistakes axi status        # full detail of the active (or most recent) run
no-mistakes axi logs --step <name> --full   # full log output of one step
no-mistakes axi abort         # cancel the active run
` + "```" + `

## Reading the output

- Output is TOON: ` + "`key: value`" + ` pairs, ` + "`name[N]{cols}:`" + ` tables, and ` + "`help[N]:`" + ` hints.
- The ` + "`help`" + ` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as ` + "`error: ...`" + ` on stdout with a ` + "`help`" + ` list; act on the suggestion.
- Exit codes: ` + "`0`" + ` success, no-op, or normal decision gates, ` + "`1`" + ` failed or cancelled final outcomes, ` + "`2`" + ` bad usage.

A ` + "`gate:`" + ` waiting on you looks roughly like this - a ` + "`gate:`" + ` line naming the
step, a ` + "`findings[N]{...}:`" + ` table with one row per finding, and a ` + "`help[N]:`" + `
list of next commands:

` + "```" + `
gate: review
findings[2]{id,severity,file,description,action}:
  r1,medium,internal/pipeline/executor.go,Error from os.Remove is ignored,auto-fix
  r2,high,cmd/no-mistakes/main.go,New --force flag bypasses the confirm prompt,ask-user
help[2]:
  no-mistakes axi respond --action fix --findings r1
  no-mistakes axi respond --action approve
` + "```" + `

Read the ` + "`action`" + ` column per row: drive ` + "`r1`" + ` (auto-fix) yourself, but stop
and escalate ` + "`r2`" + ` (ask-user) to the user before responding. A final state
instead shows ` + "`outcome: <checks-passed|passed|failed|cancelled>`" + ` with no
` + "`findings`" + ` table. Field names and exact columns can vary by step and version,
so read the actual ` + "`findings`" + ` header rather than assuming this layout.
`
