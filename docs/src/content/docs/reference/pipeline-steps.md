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
The steering still allows requested test evidence under the run's managed evidence directory, plus incidental temp or cache writes from normal development tools.
Configured shell commands and one-shot agent subprocesses are scoped to their step: when the invocation exits, fails, or is cancelled, no-mistakes terminates remaining child processes it spawned so background workers do not outlive the run.
When configured Test or Lint command output exceeds 64 KiB, the complete output remains in the authoritative step log while findings, IPC responses, and repair prompts receive a valid-UTF-8 head-and-tail projection capped at 64 KiB. The truncation marker reports the exact original and omitted byte counts and points to `no-mistakes axi logs --step <step> --full` for the complete output.
Commits created by the shared Review, Test, Document, and Lint fix path, plus CI repair commits, use the configurable [`commit.fix_message`](/no-mistakes/reference/global-config/#commitfix_message) template.
Review, Test, and Lint repair agents, including Lint's safe-fix pass when no command is configured, share a removal-first rule with the CI repair agent: when a problem can be resolved by removing a code path the intent does not strictly require, they remove it instead of validating, hardening, or documenting it. They judge necessity against user intent when present and otherwise against the change's stated purpose.
The shared correction commits, and the Push step's commit of leftover changes from a pipeline agent or formatter, are machine-authored records of pipeline output. Each is created with the complete local commit-hook family suppressed by combining `--no-verify` with an empty temporary `core.hooksPath` for that invocation, so `pre-commit`, `prepare-commit-msg`, `commit-msg`, and `post-commit` do not run. This lets a disposable run worktree commit a correction even when a tracked hook depends on generated untracked runtime files that do not exist there - the canonical case is `core.hooksPath=.husky` with a tracked hook that sources the absent `.husky/_/husky.sh`.
The suppression is limited to those correction-commit invocations. It does not change the repository, Git configuration, or daemon environment; CI repair commits and all other commit paths keep normal hook behavior. Pipeline gates remain authoritative; whether a CI repair returns through the local gates before publication is controlled by [`ci.revalidate_repairs`](/no-mistakes/reference/repo-config/#cirevalidate_repairs).
Agent roles that can write, repair, or review tests reject tests whose only evidence is matching implementation source text, tokens, syntax, or incidental snapshots.
They instead require an executable interface or a typed or normalized semantic model that proves observable behavior.
Reading a file remains valid when that file is itself an owned output or data contract, and deterministic tests may inspect the final emitted agent prompt as a generated interface; model interpretation is reserved for development-only evaluation.
Review flags every newly added violation and requires same-pattern tests encountered directly in the accepted change's scope to be removed or made semantic, without expanding the change into a repository-wide test cleanup.

## Finding decision history

When a human resolves a findings gate with Approve, Skip, or Abort without selecting a fix, no-mistakes records that the round's findings were declined. A gate with no findings records no decision. When the human selects only some findings to fix, the unselected complement is recorded as declined; findings merely left out by automatic filtering remain undecided.

Review, Test, Document, and Lint agent prompts receive a sanitized history containing the current step's earlier rounds, decisions from other steps in the same run, and a bounded window of decisions from earlier runs on the same branch. A recorded decision takes precedence over conflicting user-intent wording, and later decisions about the same concern supersede earlier ones. Completing Review does not clear branch decisions.

This context is advisory and fails open. It tells agents not to implement or re-report a declined finding unless the current code introduces a materially different problem, but it does not block a step or commit and is not a reversion detector. Rebase and CI fix prompts do not receive this decision history.

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

The integration branch used below is the [PR base branch](/no-mistakes/reference/repo-config/#prbase_branch): the repository's forge default branch, or the trusted [`pr.base_branch`](/no-mistakes/reference/repo-config/#prbase_branch) when configured.

**Behavior:**
- Fetches `origin/<PR base branch>` from the remote into the worktree, and also fetches the pushed branch for non-base branches unless the push rewrote branch history
- Without fork routing, the pushed-branch target is `origin/<branch>`
- With GitHub fork routing, the pushed-branch target is the fork branch fetched into `refs/remotes/no-mistakes-push/<branch>`
- If the branch is not the PR base branch, tries rebasing onto the pushed-branch target first, then `origin/<PR base branch>`
- If the push rewrote branch history, skips the pushed-branch rebase target so prior remote autofix commits do not get reintroduced
- If the push rewrote the PR base branch and `origin/<PR base branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- If the branch carries commits from the contributor's local default branch that are not on `origin/<PR base branch>`, pauses with an `ask-user` finding instead of silently bundling that local work into the PR
- The local-default check is best-effort and only fires when the local default tip is ahead of `origin/<PR base branch>` and is an ancestor of the branch `HEAD`
- Skips targets that don't exist or are already ancestors
- If a fast-forward is possible, does a hard-reset instead of a rebase
- If the diff against the PR base branch is empty after rebase, completes rebase and skips all remaining pipeline steps
- On conflict: records conflicting files, aborts the rebase, and reports findings
- Bounds the conflict-repair agent with [`agent_timeout`](/no-mistakes/reference/global-config/#agent_timeout): an expired budget cancels the agent and fails the step with a timeout diagnostic rather than leaving the run active indefinitely

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue` in a non-interactive Git environment so Git accepts the existing commit message instead of opening an editor. The prompt includes user intent when available. Manual fix rounds also include any per-conflict user notes, any selected user-authored findings from the TUI or AXI interface, and sanitized prior-round history in the prompt. The Rebase step does not synthesize a fix commit subject; `git rebase --continue` preserves the rebased commits' subjects.

**Default auto-fix limit:** `3`.

## Review

AI code review of your diff. This is probabilistic evidence, not a security or compliance certification, and does not replace deterministic repository-owned authorization and privacy tests, static analysis, threat modeling, or human security review.

**Behavior:**

- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Appends the [`review.path_instructions`](/no-mistakes/reference/repo-config/#reviewpath_instructions) blocks whose glob matches at least one changed file, in configured order, each labelled with its own `path` and the files it matched so a scoped rule cannot read as a repository-wide instruction; a change that matches nothing, or a repo with none configured, gets the prompt unchanged
- Selects those blocks against the complete changed-file list rather than the `ignore_patterns`-filtered one, so a pushed-branch ignore entry cannot suppress a trusted rule, and reads them from the trusted default-branch config copy regardless of `allow_repo_commands`
- Logs which of those rules it applied and which matched no changed path
- Includes user intent when the run has supplied intent or transcript matching found a relevant local agent session; the detailed provenance semantics are documented in [Intent extraction](/no-mistakes/guides/agents/#intent-extraction)
- Treats authoritative intent as enforceable for source-verifiable acceptance criteria, but does not report the absence of a remote branch, push, pull request, or CI state that this run's later Push, PR, or CI step owns
- Treats conformance with those criteria as necessary, not sufficient: an authoritative intent obliges flagging contradictions but never substitutes for checking that the algorithm is correct
- Removes any returned finding whose sole claim is that one of those same-run delivery outcomes is not present yet, while keeping findings about pre-existing or external pull requests, third-party artifacts, and lifecycle state that the current run does not own
- Keeps the later Push, PR, and CI steps responsible for strictly validating their own outcomes after review completes
- For any new or changed logic, constructs at least one concrete input or state and traces it, looking for a case that produces a wrong result without erroring; a computation that returns a wrong value, label, or set without failing is in scope
- When changed behavior processes potentially protected resources or user data, traces identity and unauthenticated reachability, authorization at the earliest shared boundary used by every caller, ownership/role/tenant/organization/admin scope and alternate call paths, public serialization, secondary disclosures through projections, caches, logs, telemetry, errors, exports, or generated artifacts, and fail-open, missing-context, preview/bypass, or stale-authorization behavior
- Reports an authorization or privacy finding only from a concrete source-backed operation or disclosure path, naming the protected resource or field, missing control, and unauthorized impact; equivalent controls and intentionally public data are accepted, and the absence of middleware, a specifically named authorization call, or an auth-related test is not evidence by itself
- Leaves access policy to repository project instructions and trusted [`review.path_instructions`](/no-mistakes/reference/repo-config/#reviewpath_instructions) rather than inventing it; when changed behavior introduces a concrete material operation or disclosure involving potentially protected resources or user data whose permissibility is not established, it must use the existing `ask-user` action and name the missing policy decision, while immaterial or pre-existing ambiguity is not reported and a source-proven routine defect retains the existing `auto-fix` semantics
- For changes that claim a durable bug fix, reconstructs the concrete failing sequence and required invariant, inspects relevant sibling paths and shared state transitions, and reports an inadequate fix only when source evidence proves the same authorized failure remains reachable; the recommendation targets the earliest supported shared boundary
- Does not treat code shape or duplication alone as evidence of a systemic defect, demand speculative redesign, block explicitly authorized short-term containment merely because a later durable fix is possible, expand the user's scope, or promote optional improvements into blockers
- Reports a finding only from a concrete sequence that occurs during the change's intended usage; a rare but real sequence under that usage still qualifies, while a hypothetical unused path that intended callers, the public API, or documented usage never take does not
- Runs a dedicated Simplification pass over every component the change introduced (a new branch, acceptance or matching path, fallback, alias, mode, flag, option, second definition of a concept the code already defines once, or parallel copy of a rule) and judges each against the stated intent, or the change's own stated purpose when no intent is available; a component not strictly required to satisfy that intent is reported as a `warning` with action `ask-user` that names the component and recommends removing it, never hardening, validating, or documenting it, and a defect finding located inside such a component names removal as its remedy. Refactor-only simplification opportunities (deduplication, clearer control flow) keep their existing meaning and never remove features
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`
- Runs every review turn - the initial review and every full rereview - as a fresh, session-free invocation, so the rereview that certifies a fix round never resumes the session whose findings prescribed those fixes; the rereview prompt additionally reframes fix-round changes as pipeline-authored code to review under the same adversarial standard as the author's changes, with prior findings, fix summaries, and same-round tests treated as claims rather than evidence; when the defects it finds sit in prior-fix-round code that exceeds what the original finding required, it reports a single `ask-user` finding recommending that round be reverted to the minimal fix instead of filing further repairs on that code
- When a review-step fixer round commits and its re-review does not complete, persists that branch's uncertified commit range (lint and document fixer commits do not); the next run's initial review of that range receives the same pipeline-authored provenance framing so the replacement reviewer is not cold. A later rebase remaps the persisted SHAs onto the rewritten head. The range is cleared only after a completed review whose approved head equals or descends from the range tip; parked, failed, skipped, and aborted reviews leave it in place
- With the default `session_reuse: true`, Claude, Codex, Grok, Pi, and Antigravity reuse one durable fixer session across review-fix turns; a resume failure retries the same fix turn in a fresh fixer session, and unsupported agents run cold
- Bounds its agent turns with [`review_agent_timeout`](/no-mistakes/reference/global-config/#review_agent_timeout): a round's optional fix turn and its rereview turn share one budget, each later auto-fix round starts a fresh one, and an expired budget cancels the agent and fails the step with a timeout diagnostic rather than leaving the run active indefinitely
- Atomically records the exact commit examined when a full review completes successfully; a parked review retains its candidate only for recovery, while failed, skipped, superseded, and legacy reviews grant no inferred approval authority

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` pause for approval instead of entering the normal auto-fix loop. This is for findings that challenge the author's intent, or whose smallest honest remedy would extend the change rather than correct it, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. With the default `auto_fix.review: 0`, blocking review findings park for approval even when their action is `auto-fix`; setting repo or global `auto_fix.review` above `0` re-enables the automatic review fix loop for eligible `auto-fix` findings. Findings with `action: no-op` are informational only. The shared [finding-action model](/no-mistakes/concepts/auto-fix/#finding-actions) owns the behavior for a missing `action`.

**Auto-fix:** the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and the shared [finding decision history](#finding-decision-history), including earlier fix summaries for this step.
The fixer fixes the reported instance narrowly, preferring to do so by addressing a deeper architectural reason and simplifying it over introducing machinery that handles the symptoms.
It follows the shared removal-first rule described above; the Review-specific guard against reverting the author's intentional code protects only code the intent requires, while genuine doubt about whether the intent requires a path leaves it in place and reports the finding unresolved.
It applies all selected fixes before running one focused verification limited to the changed area, and it is instructed not to run the complete repository test or lint suite during the fix round.
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
- Bounds those agent turns with [`test_agent_timeout`](/no-mistakes/reference/global-config/#test_agent_timeout): each evidence-gathering or Test-repair invocation gets its own budget, and an expired budget cancels the agent and fails the step with a timeout diagnostic rather than leaving the run active indefinitely
- "Do not run everything" is not "run nothing": when no targeted check can establish the intent, the agent must write or improve a focused test, perform manual verification with evidence, or report a warning finding that sufficient targeted evidence is not possible.
- The step records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the run's evidence directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR.
- Evidence is always collected under the run's evidence directory (`<NM_HOME>/evidence/<run-id>` by default, see [`test.evidence`](/no-mistakes/reference/global-config/#testevidence)), outside the worktree, so artifacts never enter the branch being validated. On GitHub.com/GHEC, [`test.evidence.attach_media`](/no-mistakes/reference/global-config/#testevidence) (default true) uploads supported image and video artifacts to GitHub user-attachments at PR render time. [`test.evidence.store_in_repo: true`](/no-mistakes/reference/global-config/#testevidence) also publishes that directory to the push-target repository's orphan evidence branch under `<test.evidence.dir>/<branch-slug>` and links the artifacts from the PR body. The config reference owns provider support and fail-closed behavior.
- Before finishing, test agents are instructed to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files under the dedicated evidence directory.
- Missing evidence for user intent can be reported as a warning with `action: ask-user`. When a host capability or OS permission is unavailable to the agent process, the agent is instructed to name the specific capability or permission and explain how to grant it before the test is rerun.
- If the agent creates new test files (detected via `git status --porcelain`), they are recorded as informational `no-op` findings and do not require approval when tests pass.

**Approval:** test findings with `action: ask-user` pause for approval, including missing-evidence warnings for user intent. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives the previous test findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and the shared [finding decision history](#finding-decision-history), including earlier fix summaries for this step. Repair mode reproduces the specific failure, applies a root-cause fix, and re-runs only focused verification - not a complete-suite confirmation - then the step's configured baseline (if any) and evidence path run again.

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
- Bounds the documentation (and combined housekeeping) agent with [`agent_timeout`](/no-mistakes/reference/global-config/#agent_timeout): an expired budget cancels the agent and fails the step with a timeout diagnostic rather than leaving the run active indefinitely

**Auto-fix:** documentation fixes happen during the initial document pass. Unresolved findings pause for approval instead of starting another automatic document/fix loop. If you manually trigger a fix from the TUI or AXI interface, the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings, and the shared [finding decision history](#finding-decision-history).

**Default auto-fix limit:** not used for automatic document follow-up loops.

## Lint

Runs linters and static analysis.

**Behavior:**

- If `commands.lint` is set: runs it via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows). Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: consumes lint-category findings from the document step's combined housekeeping pass, avoiding a second cold agent invocation. If no usable combined result exists, the lint step detects appropriate linters/formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and returns structured findings only for unresolved issues.
- Bounds those agent turns, including a configured-lint repair turn, with [`agent_timeout`](/no-mistakes/reference/global-config/#agent_timeout): an expired budget cancels the agent and fails the step with a timeout diagnostic rather than leaving the run active indefinitely

**Approval:** lint findings with `action: ask-user` pause for approval.
`action: auto-fix` findings stay eligible for the fix loop when `commands.lint` is configured.
`action: no-op` findings are informational only.
Combined-pass lint findings use the same gate: `error` and `warning` findings pause for a decision, while `info` findings do not.

**Auto-fix:** when `commands.lint` is configured, the lint step follows the same pattern as test - the agent fixes `action: auto-fix` issues using the previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and the shared [finding decision history](#finding-decision-history), including earlier fix summaries for this step, then lint re-runs.
When `commands.lint` is empty, unresolved findings from the combined pass pause for approval instead of starting another automatic lint/fix loop, because the agent already attempted safe fixes during housekeeping.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the configured push target.

**Behavior:**

- If `commands.format` is set, runs it first
- Commits any uncommitted changes left by pipeline agents or the formatter with message `no-mistakes: apply agent fixes`
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
- When the local gate mirror exists, advances its branch ref to the delivered commit when that does not rewind a newer gate submission; skips a missing mirror and fails on a divergent ref so subsequent pushes to the gate proxy remain fast-forwardable after pipeline rebases
- Only after the remote and gate mirror settle, atomically records the exact delivered commit as both the run head and successful-push binding; until that database write succeeds, the durable database head and binding remain unchanged, so a partial failure records nothing and is safe to re-enter

A remote branch can move without being rejected when all remote commits are already represented in the validated head, or when a run is intentionally rewriting history it already knew about.
Any other out-of-band commit stops the push instead of being overwritten.
Pre-skipping or later skipping Review leaves no approval binding, so Push fails closed unless Push is also skipped.

This step never requires approval - it runs automatically after review, test, document, and lint pass.

## PR

Creates or updates a pull request.

**Skipped when:**
- The branch is the [PR base branch](/no-mistakes/reference/repo-config/#prbase_branch) (the repository's forge default branch, or the trusted `pr.base_branch` when configured)
- The upstream host is not GitHub, GitLab, Forgejo, Bitbucket Cloud (`bitbucket.org`), Azure DevOps (`dev.azure.com` / `*.visualstudio.com`), or Gitea
- The provider CLI (`gh`, `glab`, `forgejo-axi`, or `tea`) is not installed for GitHub, GitLab, Forgejo, or Gitea (GitHub also skips when `gh` is missing from `PATH`)
- The provider CLI is not authenticated for GitHub, GitLab, Forgejo, or Gitea (GitHub reports a timed-out or interrupted `gh auth status` separately from auth failure; either still skips)
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)
- The `az` CLI with the `azure-devops` extension is not installed or not authenticated for Azure DevOps
- A legacy or manually edited non-GitHub repo record has `fork_url` set, because fork MR/PR routing is currently GitHub-only

**Behavior:**
- Checks for an existing PR on the branch, matching by branch alone rather than filtering by base, so a still-open PR against a since-changed [`pr.base_branch`](/no-mistakes/reference/repo-config/#prbase_branch) is found and updated instead of orphaned behind a duplicate
- If one exists, updates it. If not, creates a new one against the configured base branch, or the per-run `--base-branch` override when set.
- A per-run `--base-branch` that disagrees with an existing PR's live forge base retargets that PR (GitHub `gh pr edit --base`, GitLab `glab mr update --target-branch`, Gitea `tea api` PATCH) before updating title and body, but only the run's persisted PR URL or number after `GetPRState` proves it is still open. A sibling first-list-hit is ignored in favor of that identity; a closed or merged persisted identity, a run with no persisted identity, or a provider that cannot retarget, fails closed instead of moving another review object. A rerun inherits the selected run's PR URL only when that PR is not already merged or closed. A repo-config `pr.base_branch` change still does not retarget.
- If existing-PR discovery fails or its provider response cannot be decoded and validated as a PR listing for the configured repository, stops instead of treating the result as no PR and creating a duplicate.
- Uses `gh` for GitHub, `glab` for GitLab, `forgejo-axi` for Forgejo, `tea` for Gitea, the Bitbucket API for Bitbucket Cloud, and `az` for Azure DevOps
- For GitHub fork routing, keeps `gh --repo` pointed at the parent repository from `origin`, checks existing PRs with the bare branch name, filters matching PRs by head owner, and creates PRs with `--head <fork-owner>:<branch>`
- PR title: agent-generated from the final branch delta with user intent when available, in conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; when a scope is used, it should be the primary affected real module/package from the changed paths and kept broad rather than file-level. If drafting fails, the fallback uses the neutral title `chore: update pull request` rather than inferring scope from earlier commits.
- Bounds the PR-drafting agent with [`agent_timeout`](/no-mistakes/reference/global-config/#agent_timeout): an expired budget cancels the agent and uses that same fallback rather than leaving the run active indefinitely; a late successful title after the deadline is not used
- The PR stage exclusively owns the complete branch-scope description. It drafts `## What Changed` from the actual final diff after local mutating stages finish, and its fallback lists the final changed paths and statuses.
- PR body includes a `## Intent` section when user intent is available, the final-diff `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds. Only `## What Changed` describes the complete final branch scope; the deterministic sections remain evidence for the commit each step inspected. Auto-fix results in `## Pipeline` render as an issue -> fix -> verification narrative using captured fix summaries, re-check success text, and any still-open findings; Test details also list the recorded commands.
- `## Pipeline` keeps the existing human-readable signature and includes the stable structured step attestation documented below. Bitbucket Cloud PR descriptions omit HTML-only features (`<details>`, `<code>`, `<video>`, and the attestation comment) because Cloud renders Python-Markdown and escapes raw HTML.
- Generated PR bodies are capped at 63,488 bytes, leaving a 2 KB safety buffer below GitHub's 65,536-character body limit.
- When a body would exceed that cap, the PR step first omits older `## Pipeline` update rounds at clean update boundaries, keeps the newest rounds when possible, and points reviewers to the run log for the full pipeline history.
- Intent, `## What Changed`, risk, and testing sections are kept ahead of pipeline history; if those sections or the newest pipeline update are still too large, the PR step truncates at line or section boundaries and adds an explicit marker.
- The regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback
- Evidence artifacts render compactly in PR bodies: repository-relative `path` artifacts and `url` artifacts become `Evidence` links, `content` artifacts appear in collapsible details blocks, GitHub PRs convert repository-relative paths to blob URLs and published evidence to commit-pinned blob or raw URLs, readable UTF-8 text files from the run's evidence directory are embedded inline with truncation for large files, and binary, visual, or over-budget local artifacts render as non-link local file references
- Before the PR is created or updated, the assembled title and body pass through a final home-directory redaction. The home-directory portion of any absolute path - the operator's own home, and `/home/<user>`, `/Users/<user>`, or `C:\Users\<user>` generally - is rewritten to `~` while the rest of the path survives, so the run's evidence and worktree locations, captured command output, artifact paths, and agent prose cannot publish the operator's account name. Redaction is unconditional and runs after every length cap.
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

Items are ordered by the fixed pipeline order and represent the exact database snapshot when no-mistakes creates or updates the PR body. The attestation includes `pr` and `ci` records even though their human-readable details are not shown in `## Pipeline`; at the normal PR write point those records are commonly `running` and `pending`. The `head_sha` binds that snapshot to the commit it describes, so consumers can detect when a later push has made the comment stale. After a pipeline repair push on a host that carries that HTML attestation and can rewrite the PR body, no-mistakes rebinds an existing attestation to the newly published head so a PR that was raised through the gate stays attested; a PR that never carried an attestation is left unchanged, and hosts without a PR-body reader skip the rebind rather than failing the push.

The comment is intentionally data only. It does not declare any step required, passed for a policy, compliant, or mergeable. Consumers can parse the versioned JSON without scraping prose and apply their own policy. The comment stays with the Pipeline header when no-mistakes truncates older human-readable update details to fit a PR-body limit, and is omitted on Bitbucket Cloud.

## CI

Monitors PR health after creation and auto-fixes CI failures. Mergeability polling and merge-conflict handling apply to GitHub, GitLab, Forgejo, and Azure DevOps.

**Active for GitHub, GitLab, Forgejo, Bitbucket Cloud (`bitbucket.org`), Azure DevOps (`dev.azure.com` / `*.visualstudio.com`), and Gitea**.

- GitHub requires `gh` CLI, installed and authenticated, version >= 2.50 (older versions reject the `gh pr checks --json` call the monitor reads checks with).
- GitLab requires `glab` CLI, installed and authenticated.
- Forgejo requires `forgejo-axi`, installed and authenticated.
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.
- Azure DevOps requires the `az` CLI with the `azure-devops` extension, authenticated with a PAT.
- Gitea requires `tea` CLI, installed with a login configured for the instance.

**Behavior:**

- Polls provider CI status at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Continues its normal monitoring loop until the PR is merged, closed, declined, or the configured `ci_timeout` idle window elapses, then parks at an approval gate instead of ending the run
- If the provider check read keeps failing (6 consecutive polls while the PR is still open), parks at an ask-user approval gate instead of spinning invisibly to `ci_timeout`; the provider-neutral finding names the provider CLI or credentials and includes the underlying error (for GitHub, `gh` < 2.50 rejecting `gh pr checks --json`), and the streak resets as soon as one read succeeds
- The [`ci_timeout` reference](/no-mistakes/reference/global-config/#ci_timeout) owns idle re-arming, unlimited monitoring, and fail-closed reconciliation while that gate is parked
- On GitHub, GitLab, Forgejo, and Azure DevOps, polls provider mergeability alongside CI checks while the PR remains open
- On GitHub, combines the exact current PR head commit's check rollup with Actions workflow runs for that same commit, so a workflow rejected during validation before it creates a job or check-run still blocks readiness
- On GitHub, collapses repeated same-name runs of one workflow to the newest run that provider timestamps or Actions run identity can order. Independent workflows and same-named commit status contexts remain separate requirements, and check runs whose order cannot be established remain visible so readiness fails closed
- While the PR stays open, the TUI and terminal title show `Checks passed` once CI readiness is established and known mergeability is clear, and `no-mistakes axi` returns `outcome: checks-passed` with successful-output reporting instructions so agents can summarize the run, ask the user to review and merge, and list any pipeline fixes instead of waiting
- An empty forge check list is never treated as green unless the trusted default-branch config declares [`no_ci: true`](/no-mistakes/reference/repo-config/#no_ci). That declaration is positive durable evidence the repository intentionally has no CI; absence means CI is expected and delayed registration stays not-ready. If checks still appear on a declared no-CI repo, their actual states are honored
- If the [PR base branch](/no-mistakes/reference/repo-config/#prbase_branch) moves after `checks-passed`, keeps watching the same PR; a clean behind PR needs no action, while an actual GitHub, GitLab, Forgejo, or Azure DevOps merge conflict is auto-fixed by rebasing onto the PR base branch and re-pushing through the force-push safety guard
- Once the PR exists, its actual forge base branch (read live from the provider) takes precedence over the configured `pr.base_branch` for merge-conflict repair and base-branch tip monitoring, so a resumed run is not misled by a base-branch config change made after the PR was created
- The ready signal clears if checks start running again, new failures appear, workflow-run discovery fails or reports an unknown state, provider state otherwise becomes uncertain, or the PR is merged, closed, or declined
- If CI failures or, on GitHub, GitLab, Forgejo, or Azure DevOps, a merge conflict are already known while other checks are still pending: waits for all checks to finish before attempting an auto-fix
- Once every check has finished, classifies each terminally failed check by the provider's own reported outcome before anything escalates; [`ci.rerun_transient`](/no-mistakes/reference/repo-config/#cirerun_transient) owns which outcomes count as the provider reporting itself
- On GitHub, a positive transient rerun budget also enables structural detection of jobs that failed before any repository step ran because their setup/action-resolution phase failed, such as during a "Failed to resolve action download info" / HTTP 503 action-download outage. Detection reads the job's own setup-step conclusion (never log text) and fails closed, so an unreadable job or a real test or lint failure remains a genuine failure
- On GitHub, when the configured budget authorizes a rerun, re-runs such a check for the same commit instead of escalating it, targeting the job identified by a job link or the whole workflow identified by a cancelled run link, and naming each rerun in the step log so a run waiting on one is visible in the TUI and `axi`
- Escalates every other failure, and any merge conflict, on its first observation with no added latency, and waits out the poll or two a provider can take to publish an accepted rerun rather than escalating the outcome that rerun was meant to replace
- When a provider-attributed failure is the only remaining issue, pauses for user approval without spending an auto-fix attempt if no rerun is going to replace it. This includes a check cancelled again after its rerun and a detected GitHub setup failure that persists after its budget. On the default budget of `0`, once the budget is spent, or on a provider with no rerun API, a cancelled or stopped check itself reaches that gate. These outcomes are terminal and will not resolve on their own, there is nothing for the fix agent to repair, and the PR must not look green either
- Keeps waiting, rather than pausing, while any check can still finish on its own, so a cancellation observed alongside a running check is decided only once the rollup has stopped moving
- Never re-runs checks across a head change: if the published branch head no longer equals the commit the run delivered, the step clears any ready-to-merge signal and pauses for user approval with the expected and observed commits, because re-running checks would certify a revision this run never produced
- On CI failure: fetches failed job logs (GitHub via `gh run view --log-failed`, GitLab via `glab ci trace`, Forgejo via the exact native check target plus `forgejo-axi run view --log-failed` when runtime routes are available, Bitbucket Cloud via failed pipeline step logs; Azure DevOps has no first-class build-log command, so the agent fixes from the failing-check list without logs), sends them to the agent with user intent when available, and, if the agent produces changes, commits them with [`commit.fix_message`](/no-mistakes/reference/global-config/#commitfix_message). The fixer is told to fix a genuine code, test, or build failure and to fix that instance narrowly, preferring a deeper root cause and simplification over machinery for the symptoms, and follows the shared removal-first rule described above; it may conclude that no code change is warranted when the red check is not caused by the PR's code (a stale run, an infrastructure or attestation check such as `PR must be raised via no-mistakes` that fails only because a later pipeline push moved the head, or any failure external to the code). What happens next follows one rule on every CI-fix path: a repair is published without revalidating only when its continuity with the reviewed, published head can be proven, meaning the repaired head is the run's review-approved commit or a descendant of it. A provable repair is published immediately through the Push step's own guarded force-push path and the monitor keeps watching the same run; on GitHub, an existing pipeline attestation is rebound to that new head, and failure to settle that rewrite after retries reports the repair as unsettled instead of success. Hosts without a PR-body reader skip the rebind without failing the published repair. Anything else is held locally, the run's review approval is revoked, and validation restarts from Review so Push republishes it only after Review approves it. [`ci.revalidate_repairs`](/no-mistakes/reference/repo-config/#cirevalidate_repairs) sets the intent identically on every path: `false` (default) publishes when it is provable, `true` revalidates outright. A merge-conflict repair rebases, so its continuity is never provable and it always revalidates. Forgejo status gating remains active when logs are unsupported or unavailable
- On GitHub, includes unresolved review-thread comments from supported review bots (currently Greptile) in CI repair prompts when an auto-fix attempt starts; the comments are framed as untrusted external data and the rendered section is capped at 32 KiB
- States the configured repair policy in the step log before the first poll, so a run's log says which of the two paths a repair would take without cross-referencing the config in force at the time
- Settles the local gate mirror before atomically recording the published head and push binding, so a publication that stalls part way records nothing: the run stays on its pre-repair head and the next fix attempt re-enters the same path, finds the remote already at that commit, and completes it
- Whenever a repair revalidates - either because the setting requires it or because continuity cannot be proven - restarts at Review only: Intent and Rebase keep their results, steps already skipped for the run stay skipped, the run id is unchanged, and the durable auto-fix attempt count carries across. Earlier cycles remain in the run's round history; the step's own status shows the latest cycle
- Bounds that CI-fix agent with [`agent_timeout`](/no-mistakes/reference/global-config/#agent_timeout): an expired budget cancels the agent and fails the attempt with a timeout diagnostic rather than leaving the run active indefinitely, and a late successful return after the deadline is not committed
- If the CI-fix agent exhausts that budget, pauses for user approval instead of re-issuing the same request on the next poll. A budget burn is not transient - repeating it costs another full budget - so the remaining auto-fix attempts are left for the user to spend deliberately with a fix response. The finding carries the measured timeout diagnostic and, when the timed-out agent left uncommitted work in the run worktree, that worktree's path. Ordinary (non-timeout) fix failures keep retrying as before
- On GitHub, GitLab, Forgejo, or Azure DevOps merge conflict: asks the agent to rebase onto the latest PR base branch tip and make the smallest correct root-cause fix for the conflicts, using user intent when available
- If both CI failures and a GitHub, GitLab, Forgejo, or Azure DevOps merge conflict are present: fixes both in the same attempt
- If a fix attempt produces no changes: a trusted conclusion that the failure is not caused by the PR's code stops immediately and reports the agent's summary. Otherwise, automatic mode leaves the failure undeduplicated so a genuine code defect can retry until the auto-fix limit, while manual fix mode returns immediately for manual intervention
- Counts each automatic fix attempt durably when it starts, so revalidation or a daemon restart cannot reset the configured limit
- Exits cleanly when the PR is merged, closed, or declined
- If the idle timeout is reached while the PR is still open: pauses for user approval, even when CI checks are currently healthy
- If the idle timeout is reached while CI failures or, on GitHub, GitLab, Forgejo, or Azure DevOps, a merge conflict are still known: pauses for user approval with findings for the remaining issues
- If the idle timeout is reached while GitHub, GitLab, Forgejo, or Azure DevOps PR mergeability is still unresolved: pauses for user approval with a finding describing the unresolved mergeability state
- If CI failures or a GitHub, GitLab, Forgejo, or Azure DevOps merge conflict persist after the auto-fix limit: pauses for user approval with findings listing each failing check and/or the merge conflict

**Default auto-fix limit:** `3` total CI auto-fix attempts.

**Default transient rerun budget:** `0` reruns per provider-attributed check per run. GitHub pre-run failure detection is disabled at this value.

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
