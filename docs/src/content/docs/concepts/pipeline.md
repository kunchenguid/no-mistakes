---
title: Pipeline
description: The nine steps that run on every gated push.
---

The pipeline runs a fixed, opinionated sequence of steps. Order is not configurable. What each step runs *is*.

```
intent → rebase → review → test → document → lint → push → pr → ci
```

```mermaid
flowchart LR
  intent["Intent"] --> rebase["Rebase"] --> review["Review"] --> test["Test"] --> document["Document"] --> lint["Lint"] --> push["Push"] --> pr["PR"] --> ci["CI"]
  review -. findings .-> action["Approve / fix / skip / abort"]
  test -. findings .-> action
  document -. findings .-> action
  lint -. findings .-> action
  ci -. failures .-> action
```

This page is the overview. For each step's exact behavior, defaults, skip rules, and fix-commit format, see [Pipeline Steps](/no-mistakes/reference/pipeline-steps/).

## What a passed gate means

The pipeline is opinionated so that "passed the gate" has a stable meaning:

- the branch was checked against fresh remote upstream and the pushed-branch target first
- review, tests, user-facing test evidence when available, docs, and lint happened before any branch push to the configured target
- the human stayed in control when a step needed judgment
- the final branch update was guarded against discarding unincorporated commits already on the push target
- push, PR creation, and CI monitoring only happened after the local gate was satisfied

## The nine steps

| # | Step | What it does | Default auto-fix limit |
|---|---|---|---|
| 1 | **Intent** | Use supplied intent or infer it from recent local agent transcripts | n/a |
| 2 | **Rebase** | Fetch fresh remote upstream and the configured branch target, then rebase your branch onto them | `3` |
| 3 | **Review** | AI code review of your diff | `0` (requires approval) |
| 4 | **Test** | Run baseline tests and gather evidence for available intent | `3` |
| 5 | **Document** | Update docs when needed and report unresolved gaps | initial pass |
| 6 | **Lint** | Run lint/static analysis; shares the document step's initial housekeeping pass when no lint command is configured | `3` |
| 7 | **Push** | Safely push the validated branch to the configured target | n/a |
| 8 | **PR** | Create or update the pull request | n/a |
| 9 | **CI** | Watch CI + mergeability, auto-fix failures | `3` |

## Why these steps, in this order

- **Intent first** so downstream agent prompts and generated PR descriptions can include author intent supplied by the agent or inferred from transcripts.
- **Rebase next** so everything else runs against the latest upstream and pushed-branch target.
  It also stops when the branch would silently bundle commits from a local default branch that were never pushed to `origin/<default_branch>`.
  If there's no diff left after the rebase, the pipeline skips the rest.
- **Review before test** so the agent reads fresh code, not code it may have touched during fixes.
- **Document after test** so docs are updated against code that's known to work.
- **Lint last among local checks** so it doesn't churn over code that may still change.
- **Push → PR → CI** happens after all local checks pass.
  The push and CI auto-fix paths refuse to overwrite commits that reached the configured push target out of band.
  CI is the only step that talks to the outside world for validation.

## What each step can do

Every step can:

- **Complete** cleanly and advance the pipeline.
- **Return findings** with severity (`error`, `warning`, `info`) and an action (`auto-fix`, `ask-user`, `no-op`).
- **Trigger auto-fix** if the step's `auto_fix` limit is above 0, the step result is auto-fixable, and any finding is `auto-fix`-eligible. The document step applies safe documentation fixes during its initial pass and, when `commands.lint` is empty, combines that pass with initial safe lint fixes before the lint step consumes its findings.
- **Pause for approval** if blocking findings remain after auto-fix, or if any finding is `ask-user`.
- **Skip** when there's nothing to do (e.g., no diff, unsupported host).
- **Fail** on fatal errors and stop the pipeline.

See [Auto-Fix Loop](/no-mistakes/concepts/auto-fix/) for how the fix cycle works, and [Using the TUI](/no-mistakes/guides/tui/) for what the approval UI looks like.

## What you can configure

You can't reorder steps. You *can*:

- Swap the agent, or configure an ordered fallback list, globally or per-repo.
- Set explicit `commands.test`, `commands.lint`, `commands.format`.
- Store test evidence locally by default or opt into committed in-repo evidence with `test.evidence.store_in_repo`.
- Control auto-fix limits per step.
- Ignore paths during review and documentation checks.
- Disable or tune transcript-based intent extraction when intent is not supplied directly.
- Skip steps for one run with `no-mistakes --skip <steps>`, `git push -o no-mistakes.skip=<steps>`, `no-mistakes axi run --skip <steps>`, or from the TUI.

See [Configuration](/no-mistakes/guides/configuration/).

## What you can't configure

- The step order.
- Skipping specific steps permanently - per-run skips are allowed, but the pipeline itself always has all nine.
- Adding new steps.

This is intentional. The pipeline is opinionated so that "passed the gate" means the same thing across repos.
