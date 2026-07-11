---
title: Pipeline steps
description: Reference for each step in the validation pipeline.
---

This is the per-step reference.
For the overview and rationale, see the [pipeline concept page](/no-mistakes/concepts/pipeline/).
For how findings are repaired, see [automatic repair](/no-mistakes/concepts/auto-fix/).
For the model-selection tables behind every invocation, see the [routing reference](/no-mistakes/reference/routing/).

```
intent → rebase → review → test → document → lint → verify → push → pr → ci
```

Each step can produce findings, hand blocking findings to the routed repair cascade, pause for approval or consent, or apply safe fixes during its own pass.
Steps that hit fatal errors stop the pipeline.
Steps can also be pre-skipped when starting a run, skipped by the user, or skipped automatically by the pipeline.

Every model invocation a step makes goes through a registered purpose and the routing contract; a purpose that cannot resolve a route fails before any process launches.
In the TUI, yolo mode is the user's standing unattended consent: gates with actionable findings are fixed once with every finding selected, fix-review gates are approved, gates with only `no-op` findings are approved as-is, and an unresolved blocking repair aborts the run instead of being approved.
Every pipeline invocation is prompt-steered to keep intentional writes inside the run worktree and avoid mutating system state outside it.
This is a soft boundary, not OS-level sandbox enforcement.
The steering still allows requested test evidence under the managed temporary `no-mistakes-evidence` directory or the configured in-repo evidence directory, plus incidental temp or cache writes from normal development tools.

## Intent

Uses agent-supplied intent when a run provides it, otherwise infers the author's intent from recent local Claude Code, Codex, OpenCode, Rovo Dev, Pi, or GitHub Copilot CLI transcripts.
This is best-effort context, and when available it is included in downstream review, repair, test, documentation, lint, CI, and PR prompts.

What it does:

- uses run-supplied intent verbatim and skips transcript-based inference, even when `intent.enabled` is false
- runs transcript-based inference only when `intent.enabled` is true
- matches local agent transcripts against non-deleted changed files when present, falling back to all changed files for all-deletion diffs
- may disambiguate plausible matches with a routed `intent_disambiguation` invocation (`tools_balanced`), and summarizes the likely author intent with a routed `intent_summarization` invocation (`prose_fast`)
- restores the worktree after disambiguation, so a disambiguating model cannot leave edits behind
- stores the derived summary, source, session ID, and match score on the run
- logs accepted candidate diagnostics, including source, session, CWD, score, confidence, overlap, decision, and acceptance reason
- skips instead of failing when disabled, no matching transcript is found, the diff is empty, extraction errors, or persistence fails

This step does not block the pipeline for missing transcripts, summarization that exceeds the five-minute extraction cap, or other extraction failures, which are reported as skipped outcomes.
It can fail the run only if cleanup fails after the disambiguation invocation leaves worktree side effects.

## Rebase

Fetches the latest authoritative remote state, fetches the configured pushed-branch target, and rebases your branch onto those refs.

What it does:

- fetches `origin/<default_branch>` from the remote into the worktree, and also fetches the pushed branch for non-default branches unless the push rewrote branch history
- without fork routing, the pushed-branch target is `origin/<branch>`
- with GitHub fork routing, the pushed-branch target is the fork branch fetched into `refs/remotes/no-mistakes-push/<branch>`
- if the branch is not the default branch, tries rebasing onto the pushed-branch target first, then `origin/<default_branch>`
- if the push rewrote branch history, skips the pushed-branch rebase target so prior remote autofix commits do not get reintroduced
- if the push rewrote the default branch and `origin/<default_branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- if the branch carries commits from the contributor's local default branch that are not on `origin/<default_branch>`, pauses with an `ask-user` finding instead of silently bundling that local work into the PR
- the local-default check is best-effort and only fires when the local default tip is ahead of `origin/<default_branch>` and is an ancestor of the branch `HEAD`
- skips targets that do not exist or are already ancestors
- if a fast-forward is possible, does a hard reset instead of a rebase
- if the diff against the default branch is empty after rebase, completes the step and skips all remaining pipeline steps
- on conflict: records the conflicting files, aborts the rebase, and pauses with one finding per conflicted file

Conflict repair: a rebase conflict is never resolved silently.
The step parks, and an authorized fix routes the resolution through the `unstructured_conflict_repair` cascade, starting at `fix_balanced` and escalating to `authority_strong`.
At each tier a fresh fixer must drive the rebase to completion; the deterministic git-state checks (no rebase in progress, no unresolved conflict files) run before any adjudication, and a failed check advances the tier.
A completed resolution is then adjudicated by a separate fresh `authority_strong` verifier, and the branch and run history update only after that verification passes.
A rejected or inconclusive resolution unwinds to the pre-rebase `HEAD` and fails closed, and an exhausted cascade fails the step rather than keeping an unverified resolution.
The fixer completes the rebase in a non-interactive Git environment, so Git accepts the existing commit messages instead of opening an editor.

## Review

A fresh strong review of your diff.

What it does:

- diffs the base commit against head
- filters out files matching `ignore_patterns` from the repo config
- sends the filtered diff to a fresh `initial_review` invocation (`review_strong`) with structured review instructions and a structured output schema
- includes user intent when the run has supplied intent or transcript matching found a relevant local agent session
- returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`), plus a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`
- fails the step when the reviewer changes the candidate or returns malformed or schema-incomplete output - inconclusive output can never count as a clean strong review
- records a durable root lineage for every returned finding, tied to the exact model attempt that produced it, before any repair or approval
- seals the reviewed candidate when the review returns no blocking findings, so an unchanged candidate can later skip fresh verification

Repair: blocking `auto-fix` findings (severity `error` or `warning`) automatically enter the structured repair cascade `fix_fast → fix_balanced → authority_strong` before the gate.
Informational `auto-fix` findings take the non-blocking two-tier cascade and never gate the step.
`ask-user` findings start no fixer before consent; `no-op` findings are never repaired.
See [automatic repair](/no-mistakes/concepts/auto-fix/) for the cascade, batching, verification, and fail-closed rules.

Approval: required while any blocking finding remains unresolved, and for any `ask-user` finding.
A fix response must select explicit finding IDs; the consented repair starts at `fix_balanced`, may escalate to `authority_strong`, and must durably resolve every selected finding.
Repair commits use `no-mistakes(review): <summary>`.

## Test

Runs baseline tests and gathers evidence for the intended behavior.

What it does:

- if `commands.test` is set in repo config: runs it first as a baseline via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) and captures output; a non-zero exit pauses the step with an `error` finding and the command output
- if `commands.test` is empty, or user intent is available after the baseline command passes: a fresh `test_evidence` invocation (`tools_balanced`) validates the change with evidence-oriented tests or manual checks, returning structured findings with severity, description, and `action`
- for UI, HTML, CSS, browser, visual layout, or copy-placement changes, the evidence invocation attempts reviewer-visible visual evidence and explains in `testing_summary` when screenshots, images, videos, GIFs, or rendered HTML artifacts are not captured
- records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the temporary `no-mistakes-evidence/<runID>` directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR
- fails the step when the evidence invocation returns malformed or schema-incomplete output, so inconclusive evidence cannot pass as tested
- by default stores evidence under the temporary `no-mistakes-evidence/<runID>` directory; with `test.evidence.store_in_repo: true`, stores evidence under `<test.evidence.dir>/<branch-slug>` inside the worktree; unsafe, symlinked, or Git-ignored evidence directories fall back to temporary storage for that run
- instructs test invocations to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files
- can report missing evidence for user intent as a warning with `action: ask-user`
- requires approval when the invocation writes new test files (detected via `git status --porcelain`), even if tests pass
- commits its publishable outputs during Test - opted-in in-repo evidence and new test files - staging only those paths, so the sealed candidate carries them and Push never has to

Repair: blocking `auto-fix` test findings route through the `unstructured_test_repair` cascade, which starts at `fix_balanced` and may escalate to `authority_strong`.
A failed configured test is unstructured log evidence, so its repair never uses the fast tier merely to infer scope.
The exact configured test command is re-run as the deterministic check after each patch, and a still-failing command advances the cascade without spending a strong verifier.
`ask-user` findings, including missing-evidence warnings, pause for consent; `no-op` findings are informational only.
Repair commits use `no-mistakes(test): <summary>`.

## Document

Authors documentation updates for the code change under a placement policy.
When `commands.lint` is empty, the same authoring invocation also performs the agent-driven lint duty.
A separate fresh model then verifies the documentation before anything is committed.

What it does:

- diffs the base commit against head and skips the step when there are no non-ignored changed files to document
- leaves no lint result when it skips, so Lint runs its own pass instead of silently losing the lint duty
- uses a fresh `documentation_authoring` invocation (`prose_fast`) to update each stale fact in its authoritative owner and report only unresolved gaps or judgment calls
- applies the built-in placement policy: one authoritative owner per fact, stale duplicates removed or reduced to pointers, and no unrelated corpus rewrite
- augments the built-in policy with `document.instructions` from the trusted default-branch config when present; pushed-branch instructions cannot weaken the gate
- when `commands.lint` is empty, asks that authoring invocation to discover relevant linters and formatters, apply safe mechanical fixes, rerun the checks, and categorize unresolved findings as `documentation` or `lint`
- treats an uncategorized combined finding as documentation, so ambiguous ownership goes to the stricter gate
- fails closed on malformed or schema-incomplete author output and clears any earlier lint result before each combined pass
- records the lint category for Lint to consume once; the documentation category does not replace independent verification
- requires the author to leave `HEAD` unchanged before verification
- stages the complete authored candidate and runs the deterministic documentation check (`git diff --cached --check`)
- uses a fresh `documentation_verification` invocation (`tools_balanced`) to verify documentation accuracy, completeness, examples, configuration, public APIs, and removal of stale claims
- forbids the verifier from modifying, staging, or committing anything; a mutated candidate fails the step
- uses only the fresh documentation verifier's findings as the Document gate findings
- commits the authored changes with `no-mistakes(document): <summary>` only when verification returns no blocking findings

The documentation verifier is a documentation-only trust boundary.
It does not certify the lint half of the combined authoring pass.

Repair: blocking documentation findings route through the repair coordinator's fixed documentation policy.
The `prose_fast` author is the single-tier fixer, and a fresh `tools_balanced` documentation verifier adjudicates the patch after `git diff --check`.
An unresolved, malformed, or inconclusive verdict exhausts that finite route and fails closed rather than receiving a user-configurable retry budget.

## Lint

Runs the formatter and lint gate, commits any changes it makes, and repairs configured-command failures.
Lint is the last content mutator.
It must leave a clean worktree because the executor seals the publish candidate immediately afterwards.

What it does:

- if `commands.format` is set, runs it first and commits the result, plus any earlier uncommitted candidate changes, as `no-mistakes(lint): apply formatting`
- logs a failing formatter as a warning rather than failing the step
- if `commands.lint` is set, runs it through the platform shell; a non-zero exit pauses with a `warning`, command output, and the exact command as the repair's deterministic check
- if `commands.lint` is empty and a trusted combined result exists, consumes its lint findings once without another agent invocation
- treats errors and warnings from the combined result as blocking, while information-only findings remain visible without blocking
- does not send combined lint findings straight into automatic repair because the combined pass already attempted safe fixes
- fails closed when a combined result exists but is malformed
- if no combined result exists, runs a fresh `lint_inspection` invocation (`tools_balanced`) to detect relevant tools, apply safe fixes, rerun checks, commit changes, and report only unresolved issues
- also runs the standalone lint pass for a fix round instead of reusing a stale or consumed combined result

A combined result is in-memory and consume-once.
It does not survive a process boundary.
A skipped Document pass, an absent result, or a lint fix round therefore falls back to Lint's standalone pass.

Repair: a failing configured lint command routes blocking `auto-fix` findings through the fixed structured cascade `fix_fast → fix_balanced → authority_strong`.
The command reruns after each patch and must pass before a fresh strong verifier adjudicates the repair.
The cascade has a finite routing-owned budget and no numeric repository setting.
Agent-driven lint findings pause for approval because that pass already attempted safe fixes.
Repair commits use `no-mistakes(lint): <summary>`.

## Sealing the candidate

After Lint completes (or is skipped), the executor seals the publish candidate.
The seal records the exact `HEAD` SHA and requires a clean worktree; uncommitted changes at this point fail the run, because a mutator that leaked changes must be fixed at its source rather than swept into the published candidate.
Seals are append-only: a repaired and reverified candidate gets a new seal instead of rewriting the old one.
Verify, Push, and CI all operate on sealed SHAs, so nothing publishable ever depends on mutable worktree state.

## Verify

Gates the sealed publish candidate with a fresh aggregate verification before anything leaves the machine.

What it does:

- loads the latest sealed candidate and fails when none exists
- skips only when the sealed SHA exactly matches the latest strong-reviewed candidate - that is, nothing changed since the last clean strong review
- otherwise reviews the whole candidate diff against the base commit in a fresh invocation, paying particular attention to changes accumulated after the initial review (test fixes, documentation, formatting, lint fixes, conflict resolutions)
- verifies the exact sealed SHA: a candidate `HEAD` that no longer matches the seal fails the step
- treats inconclusive or unverifiable evidence as blocking, and fails the step on malformed or schema-incomplete verifier output
- the verifier must leave the candidate unchanged; a mutated candidate fails the step
- seals the verified SHA as the latest strong-reviewed candidate when verification returns no blocking findings

Escalation to authority: normal verification uses `normal_aggregate_verification` (`review_strong`).
Verification uses `escalated_aggregate_verification` (`authority_strong`) when the run's transient state or immutable history says the candidate crossed a higher-risk boundary:

- the run carries user intent (intent-sensitive work)
- an `authority_strong` invocation already ran in this run
- any repair verdict is inconclusive, or any repair is pending or failed
- a blocking repair did not resolve and its fixer attempt is missing or ran at the `fix_balanced` profile
- the initial review's first round rated the change high risk

Repair: blocking Verify findings route through the structured cascade with a strong aggregate verifier, exactly like review findings.
A successful Verify repair mutates the candidate, so the executor seals the repaired candidate again; unresolved or inconclusive blocking work prevents Push.
Repair commits use `no-mistakes(verify): <summary>`.

## Push

Transports the exact sealed and verified commit to the configured push target.
Push is transport only: it never formats, stages, writes evidence, or creates commits.

What it does:

- loads the latest sealed candidate and fails when none exists
- refuses to publish a dirty worktree, even when the recorded commit is unchanged
- refuses to publish when `HEAD` no longer matches the sealed SHA; a repaired candidate must be resealed and reverified before publishing
- without fork routing, the push target is `repos.upstream_url`, which comes from `origin`
- with GitHub fork routing, the push target is `repos.fork_url`
- re-reads the push target via `git ls-remote` before pushing
- for existing branches, refuses to force-push when the live remote carries commits the pipeline has not incorporated by patch-id
- fails closed when the remote safety check cannot verify whether the push would discard existing remote work
- uses `--force-with-lease=<ref>:<sha>` with an explicit SHA anchor for allowed existing-branch rewrites
- treats the branch as already pushed when the remote already points at the validated head
- uses a regular push for new branches
- updates the run's head SHA in the database after push

A remote branch can move without being rejected when all remote commits are already represented in the validated head, or when a run is intentionally rewriting history it already knew about.
Any other out-of-band commit stops the push instead of being overwritten.

This step never requires approval - it runs automatically once the sealed candidate is verified.

## PR

Creates or updates a pull request.

Skipped when:

- the branch is the default branch
- the upstream host is not GitHub, GitLab, or Bitbucket Cloud (`bitbucket.org`)
- the provider CLI (`gh` or `glab`) is not installed for GitHub or GitLab
- the provider CLI is not authenticated for GitHub or GitLab
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)
- a legacy or manually edited GitLab or Bitbucket repo record has `fork_url` set, because fork MR/PR routing is currently GitHub-only

What it does:

- checks for an existing PR on the branch; if one exists, updates it, and if not, creates a new one
- uses the provider CLI for GitHub and GitLab and the Bitbucket API for Bitbucket Cloud
- for GitHub fork routing, keeps `gh --repo` pointed at the parent repository from `origin`, checks existing PRs with the bare branch name, filters matching PRs by head owner, and creates PRs with `--head <fork-owner>:<branch>`
- composes the PR title and body with a fresh `pr_composition` invocation (`prose_fast`), using user intent when available
- PR title: conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; a scope should be the primary affected real module or package from the changed paths, kept broad rather than file-level
- PR body includes a `## Intent` section when user intent is available, a composed `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds; repair results in `## Pipeline` render as an issue → fix → verification narrative using captured fix summaries, re-check success text, and any still-open findings
- the regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback
- evidence artifacts render compactly in PR bodies: repository-relative `path` artifacts and `url` artifacts become `Evidence` links, `content` artifacts appear in collapsible details blocks, GitHub PRs convert repository-relative paths to blob URLs, readable UTF-8 text files from the temporary evidence directory are embedded inline with truncation for large files, and binary, visual, or over-budget local artifacts render as non-link local file references

Stores the PR URL in the database and streams it to the TUI.

## CI

Monitors PR health after creation and repairs CI failures through a forward-only verified republish cycle.
Mergeability polling and merge-conflict handling apply to both GitHub and GitLab.

Active for GitHub, GitLab, and Bitbucket Cloud (`bitbucket.org`):

- GitHub requires the `gh` CLI, installed and authenticated
- GitLab requires the `glab` CLI, installed and authenticated
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`

Monitoring:

- polls provider CI status at increasing intervals: every 30s for the first 5 minutes, every 60s for 5 to 15 minutes, every 120s after that
- continues monitoring an open PR until it is merged, closed, declined, or the configured `ci_timeout` idle window elapses, even after CI checks are currently healthy
- treats `ci_timeout` as an idle timeout: each upstream default-branch advance re-arms the timer, and `ci_timeout: "unlimited"` disables self-termination
- on GitHub and GitLab, polls provider mergeability alongside CI checks while the PR remains open
- while the PR stays open, the TUI and terminal title show `Checks passed` once checks are green and known mergeability is clear, and `no-mistakes axi` returns `outcome: checks-passed` with reporting instructions so agents summarize the run, ask the user to review and merge, and list any pipeline fixes instead of waiting
- the ready signal clears if checks start running again, new failures appear, provider state becomes uncertain, or the PR is merged, closed, or declined
- waits a 60s grace period before trusting empty results, because CI checks may not have registered yet
- if CI failures or, on GitHub or GitLab, a merge conflict are already known while other checks are still pending: waits for all checks to finish before attempting a repair
- exits cleanly when the PR is merged, closed, or declined

Verified republish: a hosted failure is never repaired-and-pushed in one unchecked move.
Each repair cycle:

1. Each failing check name, and a merge conflict, is a durable hosted-failure lineage with its own repair budget; distinct failures never share a budget.
2. A fresh `unstructured_ci_repair` invocation produces the patch, starting at `fix_balanced` and escalating to `authority_strong` when the same lineage fails again; provider failover stays inside a profile and never advances the tier.
3. The failing job logs (GitHub via `gh run view --log-failed`, GitLab via `glab ci trace`, Bitbucket Cloud via failed pipeline step logs) and user intent feed the fixer; a merge conflict asks for a rebase onto the current base tip with the smallest correct root-cause resolution, and combined failures are fixed in the same attempt
4. The candidate is frozen, then the configured local deterministic checks (`commands.test`, `commands.lint`) run and are journaled against every lineage in the plan; any failing check discards the candidate.
5. A separate fresh `authority_strong` verifier adjudicates the patch; blocking findings, malformed output, or an inconclusive verdict discard the candidate and fail closed.
6. A candidate that changed after verification is rejected; the verified tree is validated again at commit and at push.
7. The exact verified SHA is sealed as a `ci_republish` candidate and republished under the same force-with-lease and unseen-remote-commit protections as Push.

The republish cycle is forward-only: the executor never jumps backward to the Verify step, and an unverified patch is never pushed.

Budget and termination:

- each hosted-failure lineage has an internal finite repair budget; the budget is routing-era policy, not configuration
- a repair attempt that produces no changes is retried on later polls while budget remains in automatic mode; a manual fix that produces no changes returns immediately for manual intervention
- repair attempts are deduplicated only after a fix is actually committed and pushed
- an exhausted lineage pauses for approval with findings listing each failing check or the merge conflict, and unattended consent fails closed on it instead of approving
- profile exhaustion (all candidates unavailable) terminates the repair without advancing the quality tier
- if the idle timeout is reached while the PR is still open, while issues are still known, or while mergeability is still unresolved: pauses for approval with findings describing the remaining state

## Step statuses

Each step progresses through these statuses:

| Status | Meaning |
|---|---|
| `pending` | Not yet started |
| `running` | Currently executing |
| `fixing` | A routed fixer is repairing findings |
| `awaiting_approval` | Paused, waiting for a decision |
| `fix_review` | Paused after a fix cycle, showing results for review |
| `completed` | Finished successfully |
| `skipped` | Pre-skipped for the run, skipped by the user, or skipped automatically by the pipeline |
| `failed` | Step failed; the step log includes the returned error message so command stderr and provider errors are visible in the per-step log, not only in the daemon log |

When a non-terminal run has a step in `awaiting_approval` or `fix_review`, AXI run objects also expose `awaiting_agent: parked <duration>` as a run-level observability signal.
The signal clears as soon as the approval wait ends, including `axi respond` and cancellation, and does not change how gates resolve.
