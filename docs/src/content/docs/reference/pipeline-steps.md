---
title: Pipeline Steps
description: Reference for each step in the validation pipeline.
---

This is the per-step reference. For the overview and rationale, see [Pipeline](/no-mistakes/concepts/pipeline/). For the fix loop, see [Auto-Fix Loop](/no-mistakes/concepts/auto-fix/).

```
intent → rebase → review → test → document → lint → push → pr → ci
```

Each step can produce findings, request approval, trigger auto-fix, or apply safe fixes during its own pass. Steps that encounter fatal errors stop the pipeline. Steps can also be pre-skipped when starting a run, skipped by the user, or skipped automatically by the pipeline.
In the TUI, yolo mode is an explicit override that auto-approves steps paused for approval or fix review.
Every pipeline agent invocation is prompt-steered to keep intentional writes inside the run worktree and avoid mutating system state outside it.
This is a soft boundary, not OS-level sandbox enforcement.
The steering still allows requested test evidence under the managed temporary `no-mistakes-evidence` directory or the configured in-repo evidence directory, plus incidental temp or cache writes from normal development tools.

## Intent

Infers the author's intent from recent local Claude Code, Codex, OpenCode, Rovo Dev, or Pi transcripts.
This is best-effort context, and when available it is included in rebase fixes, review checks and fixes, test detection, evidence validation, and fixes, documentation checks and fixes, lint detection and fixes, CI auto-fixes, and PR drafting.

**Behavior:**
- Runs only when `intent.enabled` is true
- Matches local agent transcripts against non-deleted changed files when present, falling back to all changed files for all-deletion diffs, may use the configured pipeline agent to disambiguate plausible matches, and summarizes the likely author intent with that agent
- Stores the derived summary, source, session ID, and match score on the run
- Logs accepted candidate diagnostics, including source, session, CWD, score, confidence, overlap, decision, and acceptance reason
- Logs the matched source, score, and sanitized inferred intent when a transcript matches
- Skips instead of failing when disabled, no matching transcript is found, the diff is empty, extraction errors, or persistence fails

This step does not block the pipeline for missing transcripts, summarization that exceeds the five-minute extraction cap, or other extraction failures, which are reported as skipped outcomes.
It can fail the run only if cleanup fails after the disambiguation agent leaves worktree side effects.

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

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue` in a non-interactive Git environment so Git accepts the existing commit message instead of opening an editor. The prompt includes inferred user intent when available. Manual fix rounds also include any per-conflict user notes, any selected user-authored findings from the TUI, and sanitized prior-round history in the prompt. Commits use the message format `no-mistakes(rebase): <summary>`.

**Default auto-fix limit:** `3`.

## Review

AI code review of your diff.

**Behavior:**
- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Includes inferred user intent when transcript matching found a relevant local agent session
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` pause for approval and are never auto-fixed. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. Findings with `action: auto-fix` remain eligible for the fix loop. Findings with `action: no-op` are informational only.

**Auto-fix:** the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings from the TUI, and a sanitized history of prior rounds for that step, including earlier fix summaries and which findings the user left unselected. Follow-up review passes use that history to avoid re-reporting user-ignored findings unless the code now has a materially different problem. Fix commits use `no-mistakes(review): <summary>`.

**Default auto-fix limit:** `0`.

## Test

Runs baseline tests and gathers evidence for the intended behavior.

**Behavior:**
- If `commands.test` is set in repo config: runs it first as a baseline via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) and captures output. Non-zero exit produces `error` findings.
- If `commands.test` is empty, or inferred user intent is available after the baseline command passes: the agent validates the change with evidence-oriented tests or manual checks, returning structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`). For UI, HTML, CSS, browser, visual layout, or copy-placement changes, the agent attempts reviewer-visible visual evidence and explains in `testing_summary` when screenshots, images, videos, GIFs, or rendered HTML artifacts are not captured.
- The step records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the temporary `no-mistakes-evidence/<runID>` directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR.
- By default, evidence is stored under the temporary `no-mistakes-evidence/<runID>` directory. With `test.evidence.store_in_repo: true`, evidence is stored under `<test.evidence.dir>/<branch-slug>` inside the worktree, staged during push, and published with the branch. Unsafe, symlinked, or Git-ignored evidence directories fall back to temporary storage for that run.
- Before finishing, test agents are instructed to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files under the dedicated evidence directory.
- Missing evidence for inferred user intent can be reported as a warning with `action: ask-user`.
- If the agent creates new test files (detected via `git status --porcelain`), approval is required even if tests pass.

**Approval:** test findings with `action: ask-user` pause for approval, including missing-evidence warnings for inferred intent. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives the previous test findings plus any per-finding user notes, any selected user-authored findings from the TUI, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then tests run again. Fix commits use `no-mistakes(test): <summary>`.

**Default auto-fix limit:** `3`.

## Document

Updates matching documentation for code changes and reports only unresolved gaps.

**Behavior:**
- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to find every documentation gap, update docs or doc comments for all gaps it can resolve, verify its edits, and commit any documentation changes
- Includes inferred user intent when available
- Returns findings only for unresolved documentation gaps or human judgment calls
- Requires approval whenever any unresolved documentation finding is returned, including `info` findings

**Auto-fix:** documentation fixes happen during the initial document pass. Unresolved findings pause for approval instead of starting another automatic document/fix loop. If you manually trigger a fix from the TUI, the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings, and sanitized prior-round history. Fix commits use `no-mistakes(document): <summary>`.

**Default auto-fix limit:** not used for automatic document follow-up loops.

## Lint

Runs linters and static analysis.

**Behavior:**
- If `commands.lint` is set: runs it via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows). Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: the agent detects appropriate linters/formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and returns structured findings only for unresolved issues.

**Approval:** lint findings with `action: ask-user` pause for approval.
`action: auto-fix` findings stay eligible for the fix loop when `commands.lint` is configured.
`action: no-op` findings are informational only.

**Auto-fix:** when `commands.lint` is configured, the lint step follows the same pattern as test - the agent fixes `action: auto-fix` issues using the previous findings plus any per-finding user notes, any selected user-authored findings from the TUI, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then lint re-runs.
Fix commits use `no-mistakes(lint): <summary>`.
When `commands.lint` is empty, unresolved findings pause for approval instead of starting another automatic lint/fix loop, because the agent already attempted a fix during the lint pass.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the real upstream remote.

**Behavior:**
- If `commands.format` is set, runs it first
- Stages in-repo test evidence artifacts when `test.evidence.store_in_repo` is enabled and the evidence directory is not ignored by Git
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
- The upstream host is not GitHub, GitLab, or Bitbucket Cloud (`bitbucket.org`)
- The provider CLI (`gh` or `glab`) is not installed for GitHub or GitLab
- The provider CLI is not authenticated for GitHub or GitLab
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)

**Behavior:**
- Checks for an existing PR on the branch
- If one exists, updates it. If not, creates a new one.
- Uses the provider CLI for GitHub/GitLab and the Bitbucket API for Bitbucket Cloud
- PR title: agent-generated with inferred user intent when available, in conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; when a scope is used, it should be the primary affected real module/package from the changed paths and kept broad rather than file-level
- PR body includes: a `## Intent` section from extracted user intent when available, an agent-authored `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds; auto-fix results in `## Pipeline` render as an issue -> fix -> verification narrative using captured fix summaries, re-check success text, and any still-open findings
- The regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback
- Evidence artifacts render compactly in PR bodies: repository-relative `path` artifacts and `url` artifacts become `Evidence` links, `content` artifacts appear in collapsible details blocks, GitHub PRs convert repository-relative paths to blob URLs, readable UTF-8 text files from the temporary evidence directory are embedded inline with truncation for large files, and binary, visual, or over-budget local artifacts render as non-link local file references

Stores the PR URL in the database and streams it to the TUI.

## CI

Monitors PR health after creation and auto-fixes CI failures. Mergeability polling and merge-conflict handling now apply to both GitHub and GitLab.

**Active for GitHub, GitLab, and Bitbucket Cloud (`bitbucket.org`)**.

- GitHub requires `gh` CLI, installed and authenticated.
- GitLab requires `glab` CLI, installed and authenticated.
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.

**Behavior:**
- Polls provider CI status at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Continues monitoring an open PR until it is merged, closed, declined, or times out, even after CI checks are currently healthy
- On GitHub and GitLab, polls provider mergeability alongside CI checks while the PR remains open
- Waits a 60s grace period before trusting empty results (CI checks may not have registered yet)
- If CI failures or, on GitHub or GitLab, a merge conflict are already known while other checks are still pending: waits for all checks to finish before attempting an auto-fix
- On CI failure: fetches failed job logs (GitHub via `gh run view --log-failed`, GitLab via `glab ci trace`, Bitbucket Cloud via failed pipeline step logs), sends them to the agent with inferred user intent when available, and commits and force-pushes only if the agent produces changes
- On GitHub or GitLab merge conflict: asks the agent to rebase onto the latest default-branch tip and make the smallest correct root-cause fix for the conflicts, using inferred user intent when available
- If both CI failures and a GitHub or GitLab merge conflict are present: fixes both in the same attempt
- If a fix attempt produces no changes: automatic mode leaves the failure undeduplicated so it can retry until the auto-fix limit, while manual fix mode returns immediately for manual intervention
- Deduplicates fix attempts only after a fix is actually committed and pushed
- Exits cleanly when the PR is merged, closed, or declined
- If the timeout is reached while the PR is still open: pauses for user approval, even when CI checks are currently healthy
- If the timeout is reached while CI failures or, on GitHub or GitLab, a merge conflict are still known: pauses for user approval with findings for the remaining issues
- If the timeout is reached while GitHub or GitLab PR mergeability is still unresolved: pauses for user approval with a finding describing the unresolved mergeability state
- If CI failures or a GitHub or GitLab merge conflict persist after the auto-fix limit: pauses for user approval with findings listing each failing check and/or the merge conflict

**Default auto-fix limit:** `3` total CI auto-fix attempts.

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
| `skipped` | Pre-skipped for the run, skipped by the user, or skipped automatically by the pipeline |
| `failed` | Step failed; the step log includes the returned error message so command stderr and provider errors are visible in the per-step log, not only in the daemon log |
