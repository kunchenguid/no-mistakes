---
title: Pipeline Steps
description: Reference for each step in the validation pipeline.
---

This is the per-step reference. For the overview and rationale, see [Pipeline](/no-mistakes/concepts/pipeline/). For the fix loop, see [Auto-Fix Loop](/no-mistakes/concepts/auto-fix/).

```text
intent → rebase → review → test → document → lint → push → pr → ci
```

Each step can produce findings, request approval, trigger auto-fix, or apply safe fixes during its own pass. Steps that encounter fatal errors stop the pipeline. Steps can also be pre-skipped when starting a run, skipped by the user, or skipped automatically by the pipeline.
In the TUI, yolo mode is an explicit override that auto-resolves paused steps: `auto-fix` and `ask-user` findings are fixed once with every finding selected, fix-review gates are approved, and gates with only `no-op` findings are approved as-is.
Every pipeline agent invocation is prompt-steered to keep intentional writes inside the run worktree and avoid mutating system state outside it.
This is a soft boundary, not OS-level sandbox enforcement.
The steering still allows requested test evidence under the managed temporary `no-mistakes-evidence` directory, plus incidental temp or cache writes from normal development tools.
Configured shell commands and one-shot agent subprocesses are scoped to their step: when the invocation exits, fails, or is cancelled, no-mistakes terminates remaining child processes it spawned so background workers do not outlive the run.
When configured Test or Lint command output exceeds 64 KiB, the complete output remains in the authoritative step log while findings, IPC responses, and repair prompts receive a valid-UTF-8 head-and-tail projection capped at 64 KiB. The truncation marker reports the exact original and omitted byte counts and points to `no-mistakes axi logs --step <step> --full` for the complete output.
Commits created by the shared Review, Test, Document, and Lint fix path use the configurable [`commit.fix_message`](/no-mistakes/reference/global-config/#commitfix_message) template.
Agent roles that can write, repair, or review tests reject tests whose only evidence is matching implementation source text, tokens, syntax, or incidental snapshots.
They instead require an executable interface or a typed or normalized semantic model that proves observable behavior.
Reading a file remains valid when that file is itself an owned output or data contract, and deterministic tests may inspect the final emitted agent prompt as a generated interface; model interpretation is reserved for development-only evaluation.
Review flags every newly added violation and requires same-pattern tests encountered directly in the accepted change's scope to be removed or made semantic, without expanding the change into a repository-wide test cleanup.

## Intent

Uses explicit intent when a run provides it, including exact explicit intent inherited by a rerun, otherwise infers the author's intent from recent local Claude Code, Codex, OpenCode, Rovo Dev, Pi, or GitHub Copilot CLI transcripts.
This is best-effort context, and when available it is included in rebase fixes, review checks and fixes, test detection, evidence validation, and fixes, documentation checks and fixes, lint detection and fixes, CI auto-fixes, and PR drafting.

**Behavior:**

- Treats newly supplied explicit intent (`agent`) and exact inherited rerun intent (`rerun`) as authoritative acceptance criteria, while preserving their distinct sources, and skips transcript-based inference even when `intent.enabled` is false
- Runs transcript-based inference only when `intent.enabled` is true
- Matches local agent transcripts against non-deleted changed files when present, falling back to all changed files for all-deletion diffs, may use the configured pipeline agent to disambiguate plausible matches, and summarizes the likely author intent with that agent
- Stores the derived summary, source, session ID, and match score on the run
- Logs accepted candidate diagnostics, including source, session, CWD, score, confidence, overlap, decision, and acceptance reason
- Logs the matched source, score, and sanitized inferred intent when a transcript matches
- Skips instead of failing when disabled, no matching transcript is found, the diff is empty, extraction errors, or persistence fails

This step does not block the pipeline for missing transcripts, summarization that exceeds the five-minute extraction cap, or other extraction failures, which are reported as skipped outcomes.
It can fail the run only if cleanup fails after the disambiguation agent leaves worktree side effects.

## Rebase

Fetches the latest authoritative remote state, fetches the configured pushed-branch target, and rebases your branch onto those refs.

**Behavior:**

- Fetches `origin/<default_branch>` from the remote into the worktree, and also fetches the pushed branch for non-default branches unless the push rewrote branch history
- Without fork routing, the pushed-branch target is `origin/<branch>`
- With GitHub fork routing, the pushed-branch target is the fork branch fetched into `refs/remotes/no-mistakes-push/<branch>`
- If the branch is not the default branch, tries rebasing onto the pushed-branch target first, then `origin/<default_branch>`
- If the push rewrote branch history, skips the pushed-branch rebase target so prior remote autofix commits do not get reintroduced
- If the push rewrote the default branch and `origin/<default_branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- If the branch carries commits from the contributor's local default branch that are not on `origin/<default_branch>`, pauses with an `ask-user` finding instead of silently bundling that local work into the PR
- The local-default check is best-effort and only fires when the local default tip is ahead of `origin/<default_branch>` and is an ancestor of the branch `HEAD`
- Skips targets that don't exist or are already ancestors
- If a fast-forward is possible, does a hard-reset instead of a rebase
- If the diff against the default branch is empty after rebase, completes rebase and skips all remaining pipeline steps
- On conflict: records conflicting files, aborts the rebase, and reports findings

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue` in a non-interactive Git environment so Git accepts the existing commit message instead of opening an editor. The prompt includes user intent when available. Manual fix rounds also include any per-conflict user notes, any selected user-authored findings from the TUI or AXI interface, and sanitized prior-round history in the prompt. The Rebase step does not synthesize a fix commit subject; `git rebase --continue` preserves the rebased commits' subjects.

**Default auto-fix limit:** `3`.

## Review

AI code review of your diff.

**Behavior:**

- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Appends the [`review.path_instructions`](/no-mistakes/reference/repo-config/#reviewpath_instructions) blocks whose glob matches at least one changed file, in configured order, each labelled with its own `path` and the files it matched so a scoped rule cannot read as a repository-wide instruction; a change that matches nothing, or a repo with none configured, gets the prompt unchanged
- Selects those blocks against the complete changed-file list rather than the `ignore_patterns`-filtered one, so a pushed-branch ignore entry cannot suppress a trusted rule, and reads them from the trusted default-branch config copy regardless of `allow_repo_commands`
- Logs which of those rules it applied and which matched no changed path
- Includes user intent when the run has supplied intent or transcript matching found a relevant local agent session; the detailed provenance semantics are documented in [Intent extraction](/no-mistakes/guides/agents/#intent-extraction)
- Treats authoritative intent as enforceable for source-verifiable acceptance criteria, but does not report the absence of a remote branch, push, pull request, or CI state that this run's later Push, PR, or CI step owns
- Removes any returned finding whose sole claim is that one of those same-run delivery outcomes is not present yet, while keeping findings about pre-existing or external pull requests, third-party artifacts, and lifecycle state that the current run does not own
- Keeps the later Push, PR, and CI steps responsible for strictly validating their own outcomes after review completes
- For changes that claim a durable bug fix, reconstructs the concrete failing sequence and required invariant, inspects relevant sibling paths and shared state transitions, and reports an inadequate fix only when source evidence proves the same authorized failure remains reachable; the recommendation targets the earliest supported shared boundary
- Does not treat code shape or duplication alone as evidence of a systemic defect, demand speculative redesign, block explicitly authorized short-term containment merely because a later durable fix is possible, expand the user's scope, or promote optional improvements into blockers
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`
- Runs every review turn - the initial review and every full rereview - as a fresh, session-free invocation, so the rereview that certifies a fix round never resumes the session whose findings prescribed those fixes; the rereview prompt additionally reframes fix-round changes as pipeline-authored code to review under the same adversarial standard as the author's changes, with prior findings, fix summaries, and same-round tests treated as claims rather than evidence
- With the default `session_reuse: true`, Claude and Codex reuse one durable fixer session across review-fix turns; a resume failure retries the same fix turn in a fresh fixer session, and unsupported agents run cold
- Atomically records the exact commit examined when a full review completes successfully; a parked review retains its candidate only for recovery, while failed, skipped, superseded, and legacy reviews grant no inferred approval authority

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` pause for approval instead of entering the normal auto-fix loop. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. With the default `auto_fix.review: 0`, blocking review findings park for approval even when their action is `auto-fix`; setting repo or global `auto_fix.review` above `0` re-enables the automatic review fix loop for eligible `auto-fix` findings. Findings with `action: no-op` are informational only. The shared [finding-action model](/no-mistakes/concepts/auto-fix/#finding-actions) owns the behavior for a missing `action`.

**Auto-fix:** the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and which findings the user left unselected.
The fixer applies all selected fixes before running one focused verification limited to the changed area, and it is instructed not to run the complete repository test or lint suite during the fix round.
The dedicated Test and Lint steps after review remain the authoritative gates, although their coverage may be focused when commands are unconfigured.
Follow-up review passes use the history to avoid re-reporting user-ignored findings unless the code now has a materially different problem.

**Default auto-fix limit:** `0`.

### Post-review HEAD continuity

At entry to every remaining step in the fixed pipeline order - Test, Document, Lint, Push, PR, and CI - no-mistakes compares the live worktree `HEAD` with the pipeline-recorded head. An equal head or a pipeline-descendant commit continues. A backward reset, divergent sibling, or unverifiable relationship fails the run before that step performs work, including for steps that would not create a commit.

## Test

Runs **targeted** local validation of the change and requested intent, then gathers evidence for that intent.
Local Test is never a repository-wide regression-suite substitute; broad regression is owned by remote CI and remains mandatory before a PR is ready.
[`commands.test`](/no-mistakes/reference/repo-config/#commandstest) owns the configuration contract for any explicit baseline command.

**Behavior:**

- If `commands.test` is set in repo config: runs it first as a baseline via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) and captures output. Non-zero exit produces `error` findings. Configure a **targeted** command here (see repo-config); do not treat this field as CI-parity complete-suite configuration.
- If `commands.test` is empty, or user intent is available after the baseline command passes: the agent validates the change with the **smallest relevant** evidence-oriented tests or manual checks, returning structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`). Both the normal evidence agent and the Test-repair agent are instructed not to run the complete repository test suite; a generic driver instruction asking for broad or full-suite confirmation does not override that product boundary. For UI, HTML, CSS, browser, visual layout, or copy-placement changes, the agent attempts reviewer-visible visual evidence and explains in `testing_summary` when screenshots, images, videos, GIFs, or rendered HTML artifacts are not captured.
- "Do not run everything" is not "run nothing": when no targeted check can establish the intent, the agent must write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- The step records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the temporary `no-mistakes-evidence/<runID>` directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR.
- Evidence is always collected under the temporary `no-mistakes-evidence/<runID>` directory, outside the worktree, so artifacts never enter the branch being validated. On GitHub, [`test.evidence.store_in_repo: true`](/no-mistakes/reference/global-config/#testevidence) makes the PR step publish that directory to the push-target repository's orphan evidence branch under `<test.evidence.dir>/<branch-slug>` and link the artifacts from the PR body. The config reference owns provider support and fail-closed behavior.
- Before finishing, test agents are instructed to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files under the dedicated evidence directory.
- Missing evidence for user intent can be reported as a warning with `action: ask-user`.
- If the agent creates new test files (detected via `git status --porcelain`), they are recorded as informational `no-op` findings and do not require approval when tests pass.

**Approval:** test findings with `action: ask-user` pause for approval, including missing-evidence warnings for user intent. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives the previous test findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles. Repair mode reproduces the specific failure, applies a root-cause fix, and re-runs only focused verification - not a complete-suite confirmation - then the step's configured baseline (if any) and evidence path run again.

**Default auto-fix limit:** `3`.

## Document

Updates matching documentation for code changes and reports only unresolved gaps.

**Behavior:**

- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to find every documentation gap, update docs or doc comments for all gaps it can resolve, verify its edits, and commit any documentation changes under the placement policy
- The placement policy gives each fact one authoritative owner, prefers removing stale duplicates or replacing them with pointers, avoids new documentation surfaces for perceived gaps, and keeps durable incident lessons near their owner instead of in `AGENTS.md`
- `document.instructions` can add trusted default-branch ownership rules for the repository
- When `commands.lint` is empty, performs documentation and agent-driven lint in one combined housekeeping invocation, categorizing findings for the document or lint gate; if that pass is skipped, its structured output is unusable, or a daemon restart loses the in-memory result, lint runs its own agent pass instead
- Includes user intent when available
- Returns findings only for unresolved documentation gaps or human judgment calls
- Requires approval whenever any unresolved documentation finding is returned, including `info` findings

**Auto-fix:** documentation fixes happen during the initial document pass. Unresolved findings pause for approval instead of starting another automatic document/fix loop. If you manually trigger a fix from the TUI or AXI interface, the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings, and sanitized prior-round history.

**Default auto-fix limit:** not used for automatic document follow-up loops.

## Lint

Runs linters and static analysis.

**Behavior:**

- If `commands.lint` is set: runs it via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows). Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: consumes lint-category findings from the document step's combined housekeeping pass, avoiding a second cold agent invocation. If no usable combined result exists, the lint step detects appropriate linters/formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and returns structured findings only for unresolved issues.

**Approval:** lint findings with `action: ask-user` pause for approval.
`action: auto-fix` findings stay eligible for the fix loop when `commands.lint` is configured.
`action: no-op` findings are informational only.
Combined-pass lint findings use the same gate: `error` and `warning` findings pause for a decision, while `info` findings do not.

**Auto-fix:** when `commands.lint` is configured, the lint step follows the same pattern as test - the agent fixes `action: auto-fix` issues using the previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then lint re-runs.
When `commands.lint` is empty, unresolved findings from the combined pass pause for approval instead of starting another automatic lint/fix loop, because the agent already attempted safe fixes during housekeeping.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the configured push target.

**Behavior:**

- If `commands.format` is set, runs it first
- Commits any uncommitted agent changes with message `no-mistakes: apply agent fixes`
- Without fork routing, successful run-start validation selects the upstream URL from the working clone; when it matches the gate worktree's `origin`, the worktree URL is used so embedded credentials retained outside the database can authenticate. If validation fails, the run continues with its prior routing.
- With GitHub fork routing, the push target is `repos.fork_url`
- Immediately before remote mutation, reloads the durable review-approved commit and refuses to push when that binding is missing, malformed, or unreachable
- Requires the commit proposed for push to equal or descend from the review-approved commit, allowing commits made by later pipeline steps without authorizing unrelated history
- Re-reads the push target via `git ls-remote` before pushing
- For existing branches, refuses to force-push when the live remote carries commits the pipeline has not incorporated by patch-id
- Fails closed when the remote safety check cannot verify whether the push would discard existing remote work
- Uses `--force-with-lease=<ref>:<sha>` with an explicit SHA anchor for allowed existing-branch rewrites
- Pushes the exact verified commit SHA instead of mutable worktree `HEAD`
- Treats the branch as already pushed when the remote already points at that verified commit
- Uses regular push for new branches
- Updates the run's head SHA in the database to the exact commit delivered

A remote branch can move without being rejected when all remote commits are already represented in the validated head, or when a run is intentionally rewriting history it already knew about.
Any other out-of-band commit stops the push instead of being overwritten.
Pre-skipping or later skipping Review leaves no approval binding, so Push fails closed unless Push is also skipped.

This step never requires approval - it runs automatically after review, test, document, and lint pass.

## PR

Creates or updates a pull request.

**Skipped when:**

- The branch is the default branch
- The upstream host is not GitHub, GitLab, Forgejo, Bitbucket Cloud (`bitbucket.org`), or Azure DevOps (`dev.azure.com` / `*.visualstudio.com`)
- The provider CLI (`gh`, `glab`, or `forgejo-axi`) is not installed for GitHub, GitLab, or Forgejo
- The provider CLI is not authenticated for GitHub, GitLab, or Forgejo
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)
- The `az` CLI with the `azure-devops` extension is not installed or not authenticated for Azure DevOps
- A legacy or manually edited non-GitHub repo record has `fork_url` set, because fork MR/PR routing is currently GitHub-only

**Behavior:**

- Checks for an existing PR on the branch
- If one exists, updates it. If not, creates a new one.
- Uses `gh` for GitHub, `glab` for GitLab, `forgejo-axi` for Forgejo, the Bitbucket API for Bitbucket Cloud, and `az` for Azure DevOps
- For GitHub fork routing, keeps `gh --repo` pointed at the parent repository from `origin`, checks existing PRs with the bare branch name, filters matching PRs by head owner, and creates PRs with `--head <fork-owner>:<branch>`
- PR title: agent-generated from the final branch delta with user intent when available, in conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; when a scope is used, it should be the primary affected real module/package from the changed paths and kept broad rather than file-level. If drafting fails, the fallback uses the neutral title `chore: update pull request` rather than inferring scope from earlier commits.
- The PR stage exclusively owns the complete branch-scope description. It drafts `## What Changed` from the actual final diff after local mutating stages finish, and its fallback lists the final changed paths and statuses.
- PR body includes a `## Intent` section when user intent is available, the final-diff `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds. Only `## What Changed` describes the complete final branch scope; the deterministic sections remain evidence for the commit each step inspected. Auto-fix results in `## Pipeline` render as an issue -> fix -> verification narrative using captured fix summaries, re-check success text, and any still-open findings; Test details also list the recorded commands.
- `## Pipeline` keeps the existing human-readable signature and includes the stable structured step attestation documented below.
- Generated PR bodies are capped at 63,488 bytes, leaving a 2 KB safety buffer below GitHub's 65,536-character body limit.
- When a body would exceed that cap, the PR step first omits older `## Pipeline` update rounds at clean update boundaries, keeps the newest rounds when possible, and points reviewers to the run log for the full pipeline history.
- Intent, `## What Changed`, risk, and testing sections are kept ahead of pipeline history; if those sections or the newest pipeline update are still too large, the PR step truncates at line or section boundaries and adds an explicit marker.
- The regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback
- Evidence artifacts render compactly in PR bodies: repository-relative `path` artifacts and `url` artifacts become `Evidence` links, `content` artifacts appear in collapsible details blocks, GitHub PRs convert repository-relative paths to blob URLs and published evidence to commit-pinned blob or raw URLs, readable UTF-8 text files from the temporary evidence directory are embedded inline with truncation for large files, and binary, visual, or over-budget local artifacts render as non-link local file references
- For Azure DevOps, the PR description is capped at 4000 characters (UTF-16 code units, matching .NET's measurement): the agent is told about the cap and asked to keep the `## What Changed` section compact; if the assembled body still overruns, the `## Testing` section is dropped first because it can embed artifact and log content, preferentially preserving Intent, What Changed, Risk Assessment, and Pipeline; a final connector-level clamp truncates with a visible marker as a last-resort backstop

Stores the PR URL in the database and streams it to the TUI.

### Pipeline step attestation

Immediately after the existing `Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)` signature, no-mistakes writes one stable HTML comment:

```html
<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"0123456789abcdef0123456789abcdef01234567","steps":[{"step":"review","status":"completed"}]} -->
```

The `v1` payload is compact JSON with these required fields:

- `head_sha`: the exact git commit SHA recorded for the run when no-mistakes writes the PR body
- `steps`: the ordered pipeline step snapshot; every item has exactly the fields below

- `step`: the raw pipeline step name, such as `intent`, `rebase`, `review`, `test`, `document`, `lint`, `push`, `pr`, or `ci`
- `status`: the raw [step status](#step-statuses) recorded for that step, such as `completed`, `skipped`, or `failed`

Items are ordered by the fixed pipeline order and represent the exact database snapshot when no-mistakes creates or updates the PR body. The attestation includes `pr` and `ci` records even though their human-readable details are not shown in `## Pipeline`; at the normal PR write point those records are commonly `running` and `pending`. The `head_sha` binds that snapshot to the commit it describes, so consumers can detect when a later push has made the comment stale. It is not refreshed after the PR step unless no-mistakes writes the body again.

The comment is intentionally data only. It does not declare any step required, passed for a policy, compliant, or mergeable. Consumers can parse the versioned JSON without scraping prose and apply their own policy. The comment stays with the Pipeline header when no-mistakes truncates older human-readable update details to fit a PR-body limit.

## CI

Monitors PR health after creation and auto-fixes CI failures. Mergeability polling and merge-conflict handling apply to GitHub, GitLab, Forgejo, and Azure DevOps.

**Active for GitHub, GitLab, Forgejo, Bitbucket Cloud (`bitbucket.org`), and Azure DevOps (`dev.azure.com` / `*.visualstudio.com`)**.

- GitHub requires `gh` CLI, installed and authenticated.
- GitLab requires `glab` CLI, installed and authenticated.
- Forgejo requires `forgejo-axi`, installed and authenticated.
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.
- Azure DevOps requires the `az` CLI with the `azure-devops` extension, authenticated with a PAT.

**Behavior:**

- Polls provider CI status at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Continues its normal monitoring loop until the PR is merged, closed, declined, or the configured `ci_timeout` idle window elapses, then parks at an approval gate instead of ending the run
- The [`ci_timeout` reference](/no-mistakes/reference/global-config/#ci_timeout) owns idle re-arming, unlimited monitoring, and fail-closed reconciliation while that gate is parked
- On GitHub, GitLab, Forgejo, and Azure DevOps, polls provider mergeability alongside CI checks while the PR remains open
- While the PR stays open, the TUI and terminal title show `Checks passed` once CI readiness is established and known mergeability is clear, and `no-mistakes axi` returns `outcome: checks-passed` with successful-output reporting instructions so agents can summarize the run, ask the user to review and merge, and list any pipeline fixes instead of waiting
- An empty forge check list is never treated as green unless the trusted default-branch config declares [`no_ci: true`](/no-mistakes/reference/repo-config/#no_ci). That declaration is positive durable evidence the repository intentionally has no CI; absence means CI is expected and delayed registration stays not-ready. If checks still appear on a declared no-CI repo, their actual states are honored
- If the default branch moves after `checks-passed`, keeps watching the same PR; a clean behind PR needs no action, while an actual GitHub, GitLab, Forgejo, or Azure DevOps merge conflict is auto-fixed by rebasing onto the base and re-pushing through the force-push safety guard
- The ready signal clears if checks start running again, new failures appear, provider state becomes uncertain, or the PR is merged, closed, or declined
- If CI failures or, on GitHub, GitLab, Forgejo, or Azure DevOps, a merge conflict are already known while other checks are still pending: waits for all checks to finish before attempting an auto-fix
- Once every check has finished, classifies each terminally failed check by the provider's own reported outcome before anything escalates; [`ci.rerun_transient`](/no-mistakes/reference/repo-config/#cirerun_transient) owns which outcomes count as the provider reporting itself
- On GitHub, when the configured budget authorizes a rerun, re-runs such a check for the same commit instead of escalating it, targeting the job its details link identifies and naming each rerun in the step log so a run waiting on one is visible in the TUI and `axi`
- Escalates every other failure, and any merge conflict, on its first observation with no added latency, and waits out the poll or two a provider can take to publish an accepted rerun rather than escalating the outcome that rerun was meant to replace
- When cancellation is the only remaining issue, pauses for user approval without spending an auto-fix attempt if no rerun is going to replace it: a check cancelled again after its rerun, and - on the default budget of `0`, once the budget is spent, or on a provider with no rerun API - the cancellation itself. A cancellation is terminal: the provider has published its conclusion and will not replace it on its own, so continuing to poll never resolves it, there is nothing for the fix agent to repair, and the PR must not look green either
- Keeps waiting, rather than pausing, while any check can still finish on its own, so a cancellation observed alongside a running check is decided only once the rollup has stopped moving
- Never re-runs checks across a head change: if the published branch head no longer equals the commit the run delivered, the step clears any ready-to-merge signal and pauses for user approval with the expected and observed commits, because re-running checks would certify a revision this run never produced
- On CI failure: fetches failed job logs (GitHub via `gh run view --log-failed`, GitLab via `glab ci trace`, Forgejo via the exact native check target plus `forgejo-axi run view --log-failed` when runtime routes are available, Bitbucket Cloud via failed pipeline step logs; Azure DevOps has no first-class build-log command, so the agent fixes from the failing-check list without logs), sends them to the agent with user intent when available, and, if the agent produces changes, commits them and uses the same force-push safety guard as the push step. Forgejo status gating remains active when logs are unsupported or unavailable
- On GitHub, GitLab, Forgejo, or Azure DevOps merge conflict: asks the agent to rebase onto the latest default-branch tip and make the smallest correct root-cause fix for the conflicts, using user intent when available
- If both CI failures and a GitHub, GitLab, Forgejo, or Azure DevOps merge conflict are present: fixes both in the same attempt
- If a fix attempt produces no changes: automatic mode leaves the failure undeduplicated so it can retry until the auto-fix limit, while manual fix mode returns immediately for manual intervention
- Deduplicates fix attempts only after a fix is actually committed and pushed
- Exits cleanly when the PR is merged, closed, or declined
- If the idle timeout is reached while the PR is still open: pauses for user approval, even when CI checks are currently healthy
- If the idle timeout is reached while CI failures or, on GitHub, GitLab, Forgejo, or Azure DevOps, a merge conflict are still known: pauses for user approval with findings for the remaining issues
- If the idle timeout is reached while GitHub, GitLab, Forgejo, or Azure DevOps PR mergeability is still unresolved: pauses for user approval with a finding describing the unresolved mergeability state
- If CI failures or a GitHub, GitLab, Forgejo, or Azure DevOps merge conflict persist after the auto-fix limit: pauses for user approval with findings listing each failing check and/or the merge conflict

**Default auto-fix limit:** `3` total CI auto-fix attempts.

**Default transient rerun budget:** `0` reruns per cancelled check per run, before that check reaches an approval gate.

## Step statuses

Each step progresses through these statuses:

| Status | Meaning |
| --- | --- |
| `pending` | Not yet started |
| `running` | Currently executing |
| `fixing` | Agent is auto-fixing issues |
| `awaiting_approval` | Paused, waiting for user action |
| `fix_review` | Paused after a fix cycle, showing results for review |
| `completed` | Finished successfully |
| `skipped` | Pre-skipped for the run, skipped by the user, or skipped automatically by the pipeline |
| `failed` | Step failed; the step log includes the returned error message so command stderr and provider errors are visible in the per-step log, not only in the daemon log |

When a non-terminal run has a step in `awaiting_approval` or `fix_review`, AXI run objects also expose `awaiting_agent: parked <duration>` as a run-level observability signal.
The signal clears as soon as the approval wait ends, including `axi respond` and cancellation, and does not change how gates resolve.
When a step is `running` or `fixing`, AXI run objects expose an `active_steps` table with active duration, latest activity, native subprocess PID when present, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
If the latest activity is older than `step_quiet_warning`, AXI prefixes it with `quiet` to make possible wedges visible without changing the run state.
Step logs also record native subprocess start, exit, and retry lifecycle lines plus explicit auto-fix and user-fix round markers.
