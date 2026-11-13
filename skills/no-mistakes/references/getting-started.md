
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
  3. **Then validate**, passing the user's task as explicit intent. The task
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

Use an independent oracle: the expected result must come from somewhere other
than the code under test, such as a specification, worked example, published
constant, external contract, or independently justified property. Name the
public behavior, the oracle's source, and a plausible wrong behavior the test
would reject. Do not only check your own mocks, compare a result to itself, or
copy the implementation's expected-value rule: a shared mistake can pass both
sides. External-boundary mocks, constants, snapshots, and computed expectations
remain legitimate when they check an independent contract.

For a regression, reproduce the reported failure when feasible: the test should
fail before the fix and pass after it.


Everything below - preconditions, intent, the validate-and-decide loop - applies
the same way once the work is committed on a feature branch.

## Before you start

- The work you want validated must be **committed** on a branch. The gate
  validates committed history, not your uncommitted working tree.
- You must be on a **feature branch**, not the repository's default branch.
- The repository must already be initialized with `no-mistakes init`.
- The daemon must have a runnable configured pipeline agent: a supported native
  agent binary, the `agent: cursor` or `agent: devin` ACP alias, or an explicit
  `acp:<target>` through `acpx`. You are the AXI driver, not an
  implicit pipeline-agent backend. If none is available, the run fails
  before its first step; `no-mistakes doctor` reports the configuration problem.

If any of these is not met, `axi run` returns an `error:` with the exact command
to fix it - read it and act on it (commit your work, or create a branch). If the
repository is not initialized, run `no-mistakes init` first; if the `no-mistakes`
command itself is missing or misbehaving, `no-mistakes doctor` reports what is
wrong.
Before starting, run `no-mistakes axi` (home view).
If it shows an active run on your current branch, inspect it with `no-mistakes axi status`.
If it is parked at a gate, drive it with `no-mistakes axi respond`.
Reattach an in-flight run by re-running `no-mistakes axi run` when it still matches your current `HEAD` - either as the submitted head or as the current pipeline head.
Only `no-mistakes axi abort` it when you mean to discard that run before starting over; aborting is a between-runs action, never a way to take over or bypass a gate while a run is still going (see [Validate and decide](gates.md#validate-and-decide)).
If it shows an active run on another branch, leave that run alone and start validation for your current branch with `no-mistakes axi run --intent "..."`.

## Intent is required

When you start a new run you must supply explicit intent through exactly one of
`--intent TEXT`, `--intent-file PATH`, or `--intent -` (stdin until EOF):
**what the user set out to
accomplish** - the goal or request behind this work, in their terms. This is not
a description of the diff or the files you changed; it is the objective the
change is meant to achieve. You know it from the conversation, so pass it
directly - no-mistakes uses the supplied intent instead of inferring it from local
agent transcripts. Empty or whitespace-only input is rejected, never inferred.
For input handling, whitespace preservation, and reattachment semantics, see the
[CLI intent input reference](https://kunchenguid.github.io/no-mistakes/reference/cli/#intent-input).

Err on the side of completeness, not brevity. The review step uses explicit intent
to tell a deliberate decision apart from a mistake, so a thin one-line summary
makes it flag things the user already chose. Capture the nuance: the user's
goal, the specific decisions and tradeoffs they made along the way, any
constraints or approaches they ruled in or out, and anything they explicitly
asked for that might otherwise look surprising in the diff. A few sentences to a
short paragraph is normal - write down what you learned from the conversation
that a reviewer reading only the diff would not know.

