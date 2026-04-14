---
title: Pipeline Steps
description: What each step in the validation pipeline does.
---

The pipeline runs a fixed sequence of steps. The order is not configurable - this is a deliberate design choice.

```
rebase → review → test → document → lint → push → pr → babysit
```

Each step can produce findings, request approval, or trigger auto-fix. Steps that encounter fatal errors stop the pipeline. Steps can also be skipped by the user or automatically by the pipeline.

## Rebase

Fetches the latest upstream and rebases your branch onto it.

**Behavior:**
- Fetches `origin/<default_branch>` into the worktree, and also fetches `origin/<branch>` for non-default branches unless the push rewrote branch history
- If the branch is not the default branch, tries rebasing onto `origin/<branch>` first, then `origin/<default_branch>`
- If the push rewrote branch history, skips the `origin/<branch>` rebase target so prior remote autofix commits do not get reintroduced
- If the push rewrote the default branch and `origin/<default_branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- Skips targets that don't exist or are already ancestors
- If a fast-forward is possible, does a hard-reset instead of a rebase
- If the diff against the default branch is empty after rebase, completes rebase and skips all remaining pipeline steps
- On conflict: records conflicting files, aborts the rebase, and reports findings

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue`. Commits use the message format `no-mistakes(rebase): <summary>`.

**Default auto-fix limit:** `3`.

## Review

AI code review of your diff.

**Behavior:**
- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` always require human approval and are never auto-fixed. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. Findings with `action: auto-fix` remain eligible for the fix loop. Findings with `action: no-op` are informational only.

**Auto-fix:** the agent receives previous findings and applies fixes, then the review runs again. Fix commits use `no-mistakes(review): <summary>`.

**Default auto-fix limit:** `3`.

## Test

Runs your test suite.

**Behavior:**
- If `commands.test` is set in repo config: runs it via `sh -c` and captures output. Non-zero exit produces `error` findings.
- If `commands.test` is empty: the agent detects and runs relevant tests, returning structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`).
- If the agent creates new test files (detected via `git status --porcelain`), approval is required even if tests pass.

**Approval:** failing test findings with `action: ask-user` always require human approval. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives previous failure output and fixes the code for `action: auto-fix` findings, then tests run again. Fix commits use `no-mistakes(test): <summary>`.

**Default auto-fix limit:** `3`.

## Document

Checks whether the code changes need matching documentation updates.

**Behavior:**
- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to review the change and return documentation findings for any missing or stale docs, using the same `action` field as other agent-driven steps
- Requires approval whenever any documentation finding is returned, including `info` findings

**Auto-fix:** the agent updates only documentation files or doc comments, then the step re-runs and expects an empty findings list before continuing. Fix commits use `no-mistakes(document): <summary>`.

**Default auto-fix limit:** `3`.

## Lint

Runs linters and static analysis.

**Behavior:**
- If `commands.lint` is set: runs it via `sh -c`. Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: the agent detects and runs appropriate linters/formatters, returning structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`).

**Approval:** lint findings with `action: ask-user` always require human approval. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** same pattern as test - the agent fixes `action: auto-fix` issues, then lint re-runs. Fix commits use `no-mistakes(lint): <summary>`.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the real upstream remote.

**Behavior:**
- If `commands.format` is set, runs it first
- Commits any uncommitted agent changes with message `no-mistakes: apply agent fixes`
- Queries upstream via `git ls-remote` to get the current SHA for the branch
- Uses `--force-with-lease` when updating an existing branch (safe force-push that fails if the remote has diverged)
- Uses regular push for new branches
- Updates the run's head SHA in the database after push

This step never requires approval - it runs automatically after review, test, and lint pass.

## PR

Creates or updates a pull request.

**Skipped when:**
- The branch is the default branch
- The upstream host is not GitHub or GitLab
- The SCM CLI (`gh` or `glab`) is not installed
- The CLI is not authenticated

**Behavior:**
- Checks for an existing PR on the branch
- If one exists, updates it. If not, creates a new one.
- PR title: agent-generated in conventional commit format (`type(scope): description`)
- PR body includes: an agent-authored `## Summary` plus regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds

Stores the PR URL in the database and streams it to the TUI.

## Babysit

Polls CI checks on the PR and auto-fixes failures.

**Only active for GitHub** (requires `gh` CLI, authenticated).

**Behavior:**
- Polls `gh pr checks` at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Waits a 60s grace period before trusting empty results (CI checks may not have registered yet)
- On CI failure: fetches the failed run log (last 32KiB via `gh run view --log-failed`), sends it to the agent for fixing, and commits and force-pushes only if the agent produces changes
- If a fix attempt produces no changes: automatic mode leaves the failure undeduplicated so it can retry until the auto-fix limit, while manual fix mode returns immediately for manual intervention
- Deduplicates fix attempts only after a fix is actually committed and pushed
- Exits cleanly when the PR is merged or closed, or when the timeout is reached (default 4h)
- If CI failures persist after the auto-fix limit: pauses for user approval with findings listing each failing check

**Default auto-fix limit:** `3`.

## Step statuses

Each step progresses through these statuses:

| Status | Meaning |
|---|---|
| `pending` | Not yet started |
| `running` | Currently executing |
| `fixing` | Agent is auto-fixing issues |
| `awaiting_approval` | Paused, waiting for user action |
| `fix_review` | Paused after a fix cycle, showing results for review |
| `completed` | Finished successfully |
| `skipped` | User chose to skip, or the pipeline skipped it automatically |
| `failed` | Step failed |
