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
- Fetches `origin/<default_branch>` and `origin/<branch>` into the worktree
- If the branch is not the default branch, tries rebasing onto `origin/<branch>` first, then `origin/<default_branch>`
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
- Sends the filtered diff to the agent with a structured output schema
- Agent returns findings with severity (`error`, `warning`, `info`), file location, and description
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`

**Approval:** required if any finding has severity `error` or `warning`. Findings marked `requires_human_review` always require human approval and are never auto-fixed. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic.

**Auto-fix:** the agent receives previous findings and applies fixes, then the review runs again. Fix commits use `no-mistakes(review): <summary>`.

**Default auto-fix limit:** `3`.

## Test

Runs your test suite.

**Behavior:**
- If `commands.test` is set in repo config: runs it via `sh -c` and captures output. Non-zero exit produces `error` findings.
- If `commands.test` is empty: the agent detects and runs relevant tests, returning structured findings.
- If the agent creates new test files (detected via `git status --porcelain`), approval is required even if tests pass.

**Auto-fix:** the agent receives previous failure output and fixes the code, then tests run again. Fix commits use `no-mistakes(test): <summary>`.

**Default auto-fix limit:** `3`.

## Document

Checks whether the code changes need matching documentation updates.

**Behavior:**
- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to review the change and return documentation findings for any missing or stale docs
- Requires approval whenever any documentation finding is returned, including `info` findings

**Auto-fix:** the agent updates only documentation files or doc comments, then the step re-runs and expects an empty findings list before continuing. Fix commits use `no-mistakes(document): <summary>`.

**Default auto-fix limit:** `3`.

## Lint

Runs linters and static analysis.

**Behavior:**
- If `commands.lint` is set: runs it via `sh -c`. Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: the agent detects and runs appropriate linters/formatters.

**Auto-fix:** same pattern as test - agent fixes issues, lint re-runs. Fix commits use `no-mistakes(lint): <summary>`.

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
- PR body includes: a summary section, risk assessment from the review step, and a pipeline section showing each step's execution rounds

Stores the PR URL in the database and streams it to the TUI.

## Babysit

Polls CI checks on the PR and auto-fixes failures.

**Only active for GitHub** (requires `gh` CLI, authenticated).

**Behavior:**
- Polls `gh pr checks` at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Waits a 60s grace period before trusting empty results (CI checks may not have registered yet)
- On CI failure: fetches the failed run log (last 32KiB via `gh run view --log-failed`), sends to agent for fixing, commits and force-pushes
- Deduplicates fix attempts by tracking which checks failed in the last attempt
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
