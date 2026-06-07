---
name: no-mistakes
description: Validate your code changes through the no-mistakes pipeline - automated code review, tests, lint, docs, push, PR, and CI - before they reach upstream. Use when the user asks to run no-mistakes, gate or ship or validate their changes, push safely, or invokes /no-mistakes.
user-invocable: true
---

# no-mistakes

`no-mistakes` is a local gate that validates your code changes through a pipeline
(intent, rebase, review, test, document, lint, push, PR, CI) before they reach
upstream. You drive it through the `no-mistakes axi` command family, which prints
machine-readable [TOON](https://toonformat.dev) to stdout and progress to stderr.

When the user invokes `/no-mistakes`, validate the changes and report the outcome.
If the user asks for something specific, translate that request into the matching
`axi run` flags yourself - for example, "skip the lint step" becomes `--skip=lint`.
Run `no-mistakes axi run --help` to see the available flags.

## Before you start

- The work you want validated must be **committed** on a branch. The gate
  validates committed history, not your uncommitted working tree.
- You must be on a **feature branch**, not the repository's default branch.
- The repository must already be initialized with `no-mistakes init`.

If any of these is not met, `axi run` returns an `error:` with the exact command
to fix it - read it and act on it (commit your work, or create a branch).

## Intent is required

When you start a run you must pass `--intent`: **what the user set out to
accomplish** - the goal or request behind this work, in their terms. This is not
a description of the diff or the files you changed; it is the objective the
change is meant to achieve. You know it from the conversation, so pass it
directly - no-mistakes uses it verbatim instead of inferring it from local agent
transcripts (slower and flakier). One or two sentences.

## Validate and decide

Run the pipeline and decide on its findings as they come up:

1. Start the run. It blocks until the first decision point or the end:
   ```sh
   no-mistakes axi run --intent "<what the user set out to accomplish>"
   ```
2. If the output contains a `gate:` object, the pipeline is waiting on you.
   Read its `findings` table (each has an `id`, `severity`, `file`, `action`,
   and `description`) and choose one response:
   ```sh
   # accept the step as-is and continue
   no-mistakes axi respond --action approve

   # have the agent fix specific findings, then continue
   no-mistakes axi respond --action fix --findings <id1,id2> --instructions "<optional guidance>"

   # skip this step
   no-mistakes axi respond --action skip
   ```
   Each `respond` blocks until the next `gate:` or the final outcome.
3. Repeat step 2 until the output has an `outcome:` instead of a `gate:`. An
   `outcome:` of `passed` means the changes cleared the gate; `failed` or
   `cancelled` means they did not - read the output and address it.

If you have clear consent to drive the run automatically, pass `--yes` to `axi run`
or `axi respond`. It treats actionable findings as consent to fix them,
selects every current finding for one fix round, accepts the resulting fix review,
and approves gates with only `no-op` findings.

## Inspecting state

```sh
no-mistakes axi               # home view: active run, recent runs, next steps
no-mistakes axi status        # full detail of the active (or most recent) run
no-mistakes axi logs --step <name> --full   # full log output of one step
no-mistakes axi abort         # cancel the active run
```

## Reading the output

- Output is TOON: `key: value` pairs, `name[N]{cols}:` tables, and `help[N]:` hints.
- The `help` list at the bottom of most responses tells you the next commands to run.
- Errors are printed as `error: ...` on stdout with a `help` list; act on the suggestion.
- Exit codes: `0` success, no-op, or normal decision gates, `1` failed or cancelled final outcomes, `2` bad usage.
