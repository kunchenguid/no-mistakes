---
title: Repo Config Reference
description: All fields for .no-mistakes.yaml.
---

Per-repo configuration lives in `.no-mistakes.yaml` at the root of your repository.

:::caution[Security: gate-control fields are read from the default branch]
`commands.*` execute arbitrary shell on the daemon host via `sh -c` / `cmd.exe /c`, and `agent` selects which process launches there (including ordered fallback lists, ACP aliases such as `cursor`, and `acp:` targets) with the maintainer's credentials.
To prevent a supply-chain attack where a contributor lands a hostile value on a gated branch, the daemon always reads **`commands` and `agent` from your default branch** (e.g. `origin/main`), never from the pushed SHA, and reads them at the exact commit a fresh fetch resolved (so a stale `origin/<default>` ref cannot serve a value the live default branch removed).
The daemon also reads `document.instructions`, `review.path_instructions`, `protected_paths`, `disable_project_settings`, `no_ci`, `ci.rerun_transient`, `ci.revalidate_repairs`, and `test.evidence.branch` only from that trusted copy.
`pr.base_branch` is trusted-default-branch-only as well, but unlike those fields it follows the same `allow_repo_commands: true` opt-in exception as `commands`/`agent` (see [`pr.base_branch`](#prbase_branch) below).
If the default branch cannot be fetched and resolved to a readable commit, or its present `.no-mistakes.yaml` cannot be read and parsed, the run aborts before launching an agent.
A readable default-branch tree with no `.no-mistakes.yaml` is valid and uses defaults.
Commit the gate-control settings you want to your default branch.
Non-executing fields (`ignore_patterns`, `auto_fix`, `commit`, `intent`, `test`, and `providers`) are still read from the pushed branch, except `test.evidence.branch`, which names a git ref the daemon pushes to.

If you genuinely want per-branch `commands` and `agent` (for example, a single-developer repo where you trust your own feature branches), opt in with [`allow_repo_commands: true`](#allow_repo_commands) in this same file on your default branch. This re-enables the previous behavior with eyes open. The switch is read only from the trusted default-branch copy, so a contributor cannot self-enable it from a pushed branch.
:::

```yaml
# .no-mistakes.yaml

agent: codex

commands:
  lint: "golangci-lint run ./..."
  # Targeted local validation only - not a full-repo CI-parity suite.
  test: "go test ./internal/cli -run '^TestDoctor' -count=1"
  format: "gofmt -w ."

ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Optional documentation ownership policy, read only from the trusted default branch.
document:
  instructions: |
    docs/ owns detailed product guidance; README.md owns the introduction.

# Optional extra review guidance, scoped to the paths a change touches.
# Read only from the trusted default branch.
review:
  path_instructions:
    - path: "internal/scm/**"
      instructions: |
        Any URL or error string that can carry credentials must go through internal/safeurl.
    - path: "docs/**"
      instructions: |
        Prose changes only. Do not request test coverage.

# For orchestration repos whose project instructions would misidentify gate agents.
# Read only from the trusted default branch. Defaults to false.
disable_project_settings: true

# Positive declaration that this repository intentionally has no CI.
# Read only from the trusted default branch. Defaults to false (CI expected).
# no_ci: true

# Optional PR target branch, read from the trusted default branch.
# When unset, PRs target the repository's forge default branch.
pr:
  base_branch: develop

auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3

# Read only from the trusted default branch: each rerun is another workflow run,
# and revalidation decides whether a CI repair may ship without review.
ci:
  rerun_transient: 0
  revalidate_repairs: false

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: true
    attach_media: true
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence

providers:
  github:
    draft_pull_requests: false
  gitlab:
    draft_pull_requests: false
  bitbucket:
    draft_pull_requests: false
  azuredevops:
    draft_pull_requests: false
```

## Fields

### agent

Override the default agent for this repo and its setup-wizard suggestions.

| | |
| --- | --- |
| Type | `string` or `string[]` |
| Values | `auto`, `claude`, `codex`, `grok`, `rovodev`, `opencode`, `pi`, `copilot`, `antigravity`, `cursor`, `acp:<target>` |
| Default | Inherits from global config |

`auto` resolves to the first supported native agent or ACP alias in this order: `claude`, `codex`, `grok`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, `antigravity`, then `cursor`.
`cursor` is an ACP alias for the `cursor` target with default command `cursor-agent acp`.
Its availability uses the global `acpx_path` and `acp_registry_overrides.cursor` settings when present.
`acp:<target>` uses the user-installed `acpx` binary configured in global config; `acp:cursor` uses the same default command as `cursor`.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If the selected explicit agent or `auto` is unavailable, the gate fails before its first pipeline step rather than reporting partial validation as passed.

You can also set an ordered fallback list:

```yaml
agent: [codex, grok]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
After resolving `auto`, entries that resolve to the same ACP target are deduplicated in list order, so `cursor` and `acp:cursor` provide one fallback and preserve whichever spelling appears first.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.
This per-repo `agent` value, including every fallback entry, is still read from the trusted default-branch `.no-mistakes.yaml` unless `allow_repo_commands` is enabled there.

### allow_repo_commands

Opt in to honoring the code-executing selection fields (`commands.{test,lint,format}` and `agent`) from a contributor's pushed branch instead of the trusted default-branch copy.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This field is itself read **only from the trusted default-branch copy** of `.no-mistakes.yaml`, never from the pushed SHA, so a contributor cannot self-enable it by setting it on a feature branch. By default the daemon reads `commands` and `agent` from your default branch (e.g. `origin/main`) so a pushed SHA cannot inject shell or pick the launched agent on the daemon host. This opt-in covers those two fields only; `document.instructions`, `review.path_instructions`, and `disable_project_settings` stay trusted-only either way. Leave this `false` for any repo that accepts contributions. Set it to `true` only for a single-developer environment where you trust every branch you push (for example, a personal repo gated by your own daemon).

### disable_project_settings

Suppress project-level agent settings and instructions for every gate-agent start and resumed session.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

This opt-in is intended for agent-orchestration repositories whose `AGENTS.md`, `CLAUDE.md`, or harness-specific project settings would give a validation agent an operator identity and authority that it must not adopt.
When enabled, no-mistakes suppresses the target checkout's project settings for every agent-driven gate step while preserving user-level agent configuration.
Codex, Claude, and Pi are the currently verified agents: Codex receives `project_doc_max_bytes=0` and `--ignore-rules`, Claude loads only its user setting source, and Pi runs with `--no-context-files` (preserving a pinned `--no-context-files` or `-nc` spelling).
Grok 1.0.5 still discovers native project instructions and `.grok` project surfaces, so it is not a verified agent for this boundary. A configuration that resolves Grok while this option is enabled therefore fails closed before launch.
The setting applies to both new and resumed sessions.

The gate fails before launching an agent if any resolved agent or fallback lacks a verified suppression mechanism.
It also fails if `agent_args_override` defeats suppression, such as a nonzero Codex `project_doc_max_bytes` or Claude setting sources that include `project` or `local`.
When this option is `false`, missing, or `null`, all agents retain their existing project-setting behavior.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A pushed branch cannot enable it or disable a trusted opt-in.
If the trusted commit or its present config file cannot be read and parsed, the run aborts rather than guessing that the option is disabled.

### no_ci

Declare that this repository intentionally has no CI.

| | |
| --- | --- |
| Type | `bool` |
| Default | `false` |

When `true` and the forge reports **zero** checks on the PR head, the CI monitor treats that empty result as all-checks-passed and `axi run` may return `outcome: checks-passed`. The monitor log names the declaration (`no_ci: true`) so the positive evidence stays inspectable rather than silently equating every empty forge response with green.

Absence of this field means CI is expected. A zero-length check result then stays not-ready for as long as the forge reports no checks - elapsed time, grace periods, workflow-file presence or absence, prior check history, and branch names are not evidence.

If checks still appear on a declared no-CI repository, their actual states are processed normally. The declaration never waives a registered pending or failing check.

This field is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of `allow_repo_commands`.
A feature branch cannot self-declare `no_ci: true` to bypass checks, and cannot clear a trusted declaration either.

### pr.base_branch

Select the branch that newly created pull requests target.

| | |
| --- | --- |
| Type | `string` |
| Default | The repository's forge default branch |
| Trust | Trusted default branch, unless `allow_repo_commands: true` is explicitly enabled there |

Use this when the repository's integration branch differs from its forge default branch, for example `develop` instead of `main`.
The configured branch is used for PR creation, and as the integration base for the rebase step.
When unset, no-mistakes preserves the existing behavior and targets `Repo.DefaultBranch`.

PR lookup matches an existing PR by branch alone, never filtered by base, so a `pr.base_branch` change after a PR was opened updates that PR instead of opening a duplicate against the new base.
A per-run `--base-branch` override is different: if the run's already-open PR targets another branch, the PR step retargets that PR (GitHub, GitLab, and Gitea) so title, body, and CI follow the requested integration branch. A discovered PR that is not the run's persisted identity, or a provider that cannot retarget, fails closed rather than moving another review object. See [PR](/no-mistakes/reference/pipeline-steps/#pr).
Once a PR exists, its actual forge base branch is authoritative over `pr.base_branch` for the CI step's merge-conflict auto-fix and base-branch tip monitoring, protecting a resumed run from a configuration change made after the PR was created.

Because this setting controls where a PR lands, a pushed branch cannot redirect its own PR target by changing `pr.base_branch`.
It is read from the trusted default-branch copy regardless of `allow_repo_commands` by default.
The established explicit `allow_repo_commands: true` opt-in also applies to this setting for repositories that intentionally trust their pushed configuration, including a repository with no trusted default-branch copy of this file at all.
An empty value is valid and means "fall back to the forge default branch"; a non-empty value that Git would reject as a branch name fails config parsing closed, naming `pr.base_branch` in the error.

### commands.test

Explicit **targeted** local test command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (agent selects the smallest relevant tests and evidence checks) |

`commands.test` is local **targeted validation** of the change and requested intent, not a CI-parity repository-wide regression command.
Broad regression belongs in remote CI and remains mandatory before a PR is ready; do not put a complete-suite walk here just to mirror CI.
no-mistakes does not guess whether an arbitrary shell string is "too broad" - the contract is documented and dogfooded, not enforced with language- or filename-specific heuristics.

When set, the test step runs this exact command first as the baseline and checks the exit code.
When empty, the agent detects and runs the smallest relevant tests itself (and is instructed never to run the complete repository suite).
When user intent is available, the agent may still run after a successful baseline command to gather evidence-oriented validation, still under the same targeted-validation contract.

### commands.lint

Explicit lint command. Run via the platform shell - `sh -c` on POSIX, `cmd.exe /c` on Windows.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (agent auto-detects) |

When set, the lint step runs this exact command and checks the exit code.
When empty, the agent-driven lint duty is folded into the document step's combined housekeeping pass: one agent invocation covers both documentation and lint, and the lint step consumes that result, reporting lint-category findings with the same gate semantics (blocking findings park for a decision).
Neither responsibility is skipped: when the document step has nothing to run against (or its structured output cannot be trusted), the lint step runs its own agent pass as before.

### commands.format

Formatter command run before the push step commits agent fixes.

| | |
| --- | --- |
| Type | `string` |
| Default | Empty (no separate push-step formatter) |

This does not prevent empty `commands.lint` from detecting and running formatters during the combined housekeeping pass, or during the lint step when that pass cannot provide a result.

### document.instructions

Repository-specific documentation ownership policy for the document step.

| | |
| --- | --- |
| Type | `string` (multiline) |
| Default | Empty (built-in placement policy only) |

The document step always applies a built-in placement policy: every fact has exactly one authoritative owner document, stale duplicates are removed or reduced to pointers instead of synchronized, no new documentation surfaces are created merely to close perceived gaps, and incident lessons live as invariants near their owner (with a pointer to the regression test), never as AGENTS.md postmortems.
`document.instructions` states this repository's ownership map or extra placement rules (for example, which file owns which class of facts).
It augments or clarifies the built-in policy; it cannot disable documentation integrity.

Like `commands.*` and `agent`, this field steers gate behavior, so it is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`: a contributor's pushed branch cannot weaken the documentation rules that gate its own review.

### review.path_instructions

Extra review guidance, scoped to the paths a change actually touches.

| | |
|---|---|
| Type | `object[]` with `path` (`string`) and `instructions` (`string`, multiline) |
| Default | Empty (built-in review instructions only) |

Use this for house rules that only apply to part of the tree, for example a redaction rule for the code that builds remote URLs, or a note that a documentation directory needs no test coverage:

```yaml
review:
  path_instructions:
    - path: "internal/scm/**"
      instructions: |
        Any URL or error string that can carry credentials must go through internal/safeurl.
    - path: "docs/**"
      instructions: |
        Prose changes only. Do not request test coverage.
```

Each matched rule reaches the reviewer with the scope it was selected for, so a rule scoped to one directory can never read as a repository-wide instruction:

```
path: docs/**
matched files: docs/notes.md
instructions:
Prose changes only. Do not request test coverage.
```

#### Matching

`path` uses the same matcher and syntax as [`ignore_patterns`](#ignore_patterns), including the rule that `*` never crosses a `/`, so `**/*.go` covers a single directory level rather than every Go file.

The review step appends only the blocks whose `path` matches at least one changed file, in the order they appear in the file.
Two entries with the same `path` **and** the same `instructions` are injected once. The same instruction text under two different `path` values is injected once per path, because each block states its own scope. Two entries with the same `path` and different `instructions` are both injected.
Matching runs against the full changed-file list and is deliberately **not** filtered by `ignore_patterns`: that field is read from the pushed branch, so filtering here would let a contributor drop one of your rules from the review of their own branch.

Blocks augment the built-in review instructions; they cannot disable them, and a finding the reviewer raises from a block goes through the same severity and action model as any other finding.
With nothing configured, or nothing matching the change, the review prompt is exactly what it would be without this setting.
The step log names the rules it applied and the rules that matched nothing, so a rule that never fires is visible in `no-mistakes axi logs --step review`.

#### Limits and validation

`instructions` is prompt text, so merge-conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`) are removed from it and runs of whitespace are collapsed, exactly as for [`document.instructions`](#documentinstructions). Write rules without those tokens; a value that would be left empty once they are removed is rejected rather than silently dropped.

At most 32 entries are allowed, and the assembled prompt section may not exceed 16,384 bytes, because the injected text shares the review prompt's budget and an oversized prompt fails the agent invocation outright.
The size is measured on what is actually injected: the heading, and for every entry its labels, its `path`, its `instructions`, and a 192-byte allowance for its matched-file list. A block whose matched-file list would exceed that allowance is truncated with a `+N more` suffix, so the measured limit holds for any diff.

A missing `path` or `instructions` value, an `instructions` value that renders empty, a `path` that is not a valid glob, or a config over either limit fails when the config is parsed, so the run aborts before an agent starts instead of silently dropping guidance.
These checks run on whichever copy of the file is parsed, including the pushed branch's. A pushed branch's blocks are ignored when the review prompt is built (see [Trust](#trust) below), but an invalid block on that branch still fails its own run, so a broken rule surfaces before it merges and becomes the trusted copy.

#### Trust

Like `document.instructions`, this field steers gate behavior, so it is honored **only from the trusted default-branch copy** of `.no-mistakes.yaml`, regardless of [`allow_repo_commands`](#allow_repo_commands): a value present only on a pushed branch is ignored, so a contributor cannot inject instructions into the review that gates them.

### Command process lifetime

All configured `commands.*` entries are scoped to their step.
After no-mistakes starts one of these commands, it terminates any remaining child processes from that command when the command exits, fails, or the step is cancelled.
Do not rely on a configured command to leave a background server or watcher running after it returns; keep that service inside the command lifetime or start it outside no-mistakes.

### ignore_patterns

Paths to exclude from review and documentation checks.

| | |
| --- | --- |
| Type | `string[]` |
| Default | Empty (no ignores) |

Pattern matching rules. [`review.path_instructions`](#reviewpath_instructions) uses the same matcher, so there is one path syntax to learn:

| Pattern | Rule |
| --- | --- |
| `*.generated.go` | No slash - matches by basename, at any depth |
| `vendor/**` | Ends with `/**` - matches that directory and everything under it |
| `some/path/file.go` | Contains a slash - full path glob against the whole path |
| `**/*.go` | Also a full path glob, so **only one directory level** - `internal/main.go`, not `internal/scm/github/github.go` |

`*` never crosses a `/`, on every platform, so `**/*.go` is not "every Go file"; it behaves as a single-segment wildcard. Use `*.go` to match by extension at any depth, or `internal/**` to cover a subtree.

### protected_paths

Opt-in paths that automatic commits must leave for an operator to resolve.

| | |
| --- | --- |
| Type | `string[]` |
| Default | Empty (no protected paths) |
| Trust | Trusted default branch only, regardless of `allow_repo_commands` |

```yaml
protected_paths:
  - "package-lock.json"
  - "*.lock"
  - ".github/**"
```

Patterns use the same syntax as [`ignore_patterns`](#ignore_patterns). Empty or malformed rules fail config loading. Commit this setting to the default branch to enable it; a pushed branch cannot add, remove, or replace the trusted policy for its own run.

Before staging an automatic Review, Test, Document, Lint, CI repair, or Push leftover commit, the pipeline checks the index and worktree for dirty protected paths. This includes staged and unstaged modifications, deletions, both ends of renames, and individual untracked files inside new directories. A match refuses the entire commit and parks the step at an operator approval gate naming the path and rule. The index and all working files stay as they were; nothing is restored, unstaged, discarded, or partially committed. The Push check also covers formatter changes and residue from earlier steps. [Daemon & Worktrees](/no-mistakes/concepts/daemon/#what-it-does) owns retention across terminal cleanup and crash recovery.

A protected-path refusal always requires an explicit response, including under AXI `--yes` and TUI yolo mode. CI does not retry its fixer, and automatic gate reconciliation cannot clear the refusal, even if the PR closes. Approval is rejected because it would skip unfinished work: inspect and resolve the reported edit, then use `no-mistakes axi respond --action fix` to retry the step, including its commit and publication. Deliberate skip and abort behavior is unchanged.

For CI, that explicit fix finishes the retained repair before normal monitoring can report `checks-passed`, even if forge checks turned green while parked or the response selects only an added finding. It uses the existing [repair revalidation policy](#cirevalidate_repairs), including mandatory revalidation from Review for a rebased conflict repair, and the existing publication guards. If the retained repair cannot finish, the refusal remains available for another explicit fix response.

This is a staging guard, not an agent filesystem sandbox or a check on semantic intent. It does not inspect changes already committed by the author or an agent, and it does not infer whether an unprotected edit belongs to a finding. With an empty list, automatic staging keeps its existing behavior. `ignore_patterns` only filters checks and does not prevent staging.

### auto_fix

Override auto-fix attempt limits for specific steps. Fields not set here inherit from global config.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default |
| --- | --- | --- |
| `auto_fix.rebase` | `int` | Inherits from global (default `3`) |
| `auto_fix.review` | `int` | Inherits from global (default `0`) |
| `auto_fix.test` | `int` | Inherits from global (default `3`) |
| `auto_fix.document` | `int` | Inherits from global (default `3`) |
| `auto_fix.lint` | `int` | Inherits from global (default `3`) |
| `auto_fix.ci` | `int` | Inherits from global (default `3`) |

Set to `0` to disable the follow-up auto-fix loop for a step (findings require manual approval).
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings pause for approval instead of starting another automatic fix loop.

`auto_fix.ci` covers the CI step's CI failure and merge-conflict auto-fix attempts.

Legacy alias: `auto_fix.babysit`.

### ci.rerun_transient

How many times the CI step may re-run a single provider-attributed check before that check reaches an approval gate.
This covers cancellations on supported providers and, when the value is positive, opts GitHub into detecting jobs that failed before any repository step ran.

| | |
|---|---|
| Type | `int` |
| Default | `0` |
| Range | `0` to `5`; values outside it are clamped |
| Trust | Read only from the trusted default branch |

Every rerun this budget authorizes is another provider-side workflow run billed to the repository, so the value is read only from the trusted default-branch copy of this file, exactly like `document.instructions` and `disable_project_settings`.
A pushed branch cannot raise its own rerun budget.
The default is `0` because a cancelled conclusion does not identify its cause: the same value covers the provider aborting its own infrastructure, a maintainer stopping a runaway or unsafe job, and repository concurrency with `cancel-in-progress`.
Rerunning on that ambiguity can restart work someone deliberately stopped, so raise this only for a repository whose cancellations are known to be provider-side.
At `0`, no-mistakes makes no extra provider call to classify a GitHub setup failure, so that failure keeps the earlier CI failure and auto-fix behavior.

With no trusted copy of this file, the operator's own [`ci.rerun_transient`](/no-mistakes/reference/global-config/#cirerun_transient) applies, then the built-in default.
A value set here always wins over the global one, so the maintainer of the repository has the last word on how many workflow runs their project is billed for.

With a positive budget, a rerun is requested when the provider attributes the outcome to itself rather than to the job, which is true in two cases:

- The provider reported the outcome as `cancelled`, the one terminal conclusion it attributes to itself rather than to the job.
- On GitHub, the job failed before any repository step ran because its setup/action-resolution phase failed, for example during a "Failed to resolve action download info" / HTTP 503 outage while downloading the actions the job uses. This is read structurally from the job's own setup-step conclusion, never from log text, so it cannot mask a real failure: a genuine test or lint failure cleared setup and failed a later step. When the detected setup failure persists past the budget, it reaches the same approval gate as an unresolved cancellation rather than the fix agent. An unreadable or unmatched job fails closed and remains an ordinary failure.

The remaining outcomes are the job's own verdict on the commit and are never re-run:

- `failure`, `error`, `action_required`, and `startup_failure` (after any repository step ran) are the job's verdict, so they escalate on the first failure with no added latency.
- `timed_out` means the job exceeded its own `timeout-minutes`, which is usually the branch's own code hanging. Re-running it burns another full timeout window reproducing the same failure, so it is treated as a genuine failure and is not opt-in.
- `stale` is already treated as skipped rather than failed, so it never reaches this decision.
- An outcome no-mistakes recognizes as none of the above never earns a rerun either.

A single genuine job failure, or a merge conflict, suppresses the rerun for that poll: the fix agent is needed regardless, and no rerun can clear a merge conflict.

The budget is per check per run and is spent when the rerun is requested, so a provider that refuses the request cannot be retried in a loop.
Check names are not unique on a pull request, so same-named checks share one budget.

A rerun request returns as soon as the provider accepts it, while the new attempt replaces the provider-attributed check in the status rollup a moment later.
A poll that still reads the exact completion the rerun was requested for has observed nothing new, so the monitor waits for a bounded couple of polls rather than escalating a check it never actually re-ran.
A provider that accepts a rerun and never publishes it cannot stall the run past that.
Once the provider publishes a conclusive replacement, no-mistakes durably stops treating that rerun as outstanding while preserving the spent budget; if the exact watched head is then green, the monitor reports `checks-passed` normally.

A provider-attributed check that no rerun is going to replace pauses the step for user approval when it is the only remaining issue, so the pull request never looks green.
That includes a check that came back cancelled after its rerun and a detected GitHub setup failure that persisted after its budget.
At the default budget of `0`, once the budget is spent, or on a provider with no rerun API, cancellation itself reaches this gate because the provider has published its conclusion and will not publish another one on its own.
The check does not enter the `auto_fix.ci` loop and never consumes an auto-fix attempt: it is not a verdict on the code, so there is nothing for the fix agent to repair and no reason to let it edit code the provider never tested.
Answering that gate with `fix` is still honored, and the fix round you asked for is told about the check alongside any other issue.

Reruns are skipped when:

- The provider has no rerun API (only GitHub implements one today; GitLab, Forgejo, Bitbucket Cloud, Azure DevOps, and Gitea reach the approval gate without a rerun).
- The check's details link names nothing the provider can re-run, for example a third-party status pointing at an external dashboard, or a link under a workflow run that names no job the API accepts. A link naming one job re-runs that job; a cancelled check naming only the workflow run re-runs the whole workflow, while other run-only links re-run failed jobs; an unrecognized link is widened into neither.
- The published branch head no longer equals the commit the run delivered. That case terminates with the expected and observed commits instead: re-running checks against a different head would certify a revision this run never produced. See [pipeline steps: CI](/no-mistakes/reference/pipeline-steps/#ci).

### ci.revalidate_repairs

Whether every CI repair must re-pass the pipeline before it is published, or only the ones whose continuity with the reviewed head cannot be proven.

| | |
|---|---|
| Type | `bool` |
| Default | `false` |
| Trust | Read only from the trusted default branch |

```yaml
ci:
  revalidate_repairs: true
```

One rule decides how every CI repair is delivered, on every CI-fix path - automatic and manual, CI failure and merge conflict alike:

> A repair is published without revalidating only when its continuity with the reviewed, published head can be **proven**. When that continuity cannot be proven, the repair revalidates from Review.

Continuity is proven when the repaired head is the run's durably review-approved commit or a descendant of it. That is the same fact the Push step's publication guard enforces, so the decision to publish and the guard that permits the push can never disagree.

`revalidate_repairs` sets the intent, identically on every path:

- **`false` (default)** asks to publish when it is safe to. A repair that builds on the reviewed head - the ordinary case, where the fix agent adds a commit - is committed and published immediately through the same guarded path the [Push step](/no-mistakes/reference/pipeline-steps/#push) uses (review-approved-head continuity, the force-with-lease anchor, remote verification, and the durable push binding all still apply), and the CI monitor keeps watching the same run for the new head. One repair costs one agent round.
- **`true`** asks for revalidation outright: every repair is kept local, the run's review approval is revoked, and validation restarts at Review so the repaired head re-passes Review, Test, Document, and Lint before Push republishes it.

CI repair publication uses the same settlement order as Push. The [CI step reference](/no-mistakes/reference/pipeline-steps/#ci) owns the publication and retry behavior.

**Merge-conflict repairs always revalidate, under either setting.** They are not carved out - they simply always land in the cannot-be-proven half. A conflict repair rebases, so the repaired head is never a descendant of the reviewed head; resolving a conflict changes the commit's patch-id; and no content-based guard can separate "rebased and resolved" from "dropped the work". Revalidating is what keeps that safe: the rewritten head is not published until Review has approved it, so the reviewed commits stay on the remote in the meantime.

Provenance is deliberately not accepted as a substitute for that proof. In the reproduction this rule exists for, the repair that deleted a reviewed commit was authored by no-mistakes' own CI repair agent: it reset to the rebase base, left a clean tree, and the pipeline reported success while the remote lost the work. Who wrote a repair says nothing about what it did to the reviewed commits.

The tradeoff `true` buys is cost against an unreviewed repair:

| | `false` (default) | `true` |
|---|---|---|
| Ordinary repair that builds on the reviewed head | published immediately, one agent round | revalidated: one agent round plus a full Review, Test, Document, Lint, Push, PR pass |
| Merge-conflict repair | revalidated | revalidated |
| Ordinary repair is reviewed before it reaches the PR | no | yes |
| Steps that re-run when a repair revalidates | Review onward; Intent and Rebase do not | same |
| Run identity | unchanged; a restart is a same-run rewind | same |

Turn it on where even an ordinary unreviewed CI repair is unacceptable.
The concrete case this exists for: when a review bot posts product-behavior findings as a failing check, the fix agent treats them as CI failures and can reverse what the change was supposed to do.
On [firstmate#3250](https://github.com/kunchenguid/firstmate/pull/3250) a CI repair made a `--changed` test run serial by default, contradicting the change's stated intent; the restarted Review caught it and reversed it. Without revalidation that repair would have shipped.
That is the safety this option buys, and the reason it is offered rather than removed.

This value is read only from the trusted default-branch copy of this file, like `ci.rerun_transient` and `disable_project_settings`.
A pushed branch cannot turn a maintainer's revalidation requirement off for its own repairs, and cannot turn it on either.

A value set here always wins over the operator's own [`ci.revalidate_repairs`](/no-mistakes/reference/global-config/#cirevalidate_repairs), in both directions: `true` here enables revalidation even when the global value is `false`, and an explicit `false` here opts out even when the global value is `true`.
With no trusted copy of this file, the operator's global value applies, then the built-in default of `false`.

### commit.fix_message

Override the auto-fix commit subject template for this repository.

| | |
| --- | --- |
| Type | `string` |
| Default | Inherits from global config, whose default is `no-mistakes({{.Step}}): {{.Summary}}` |

The value follows the [global `commit.fix_message` template syntax and validation rules](/no-mistakes/reference/global-config/#commitfix_message).
That includes the 1,024-byte template limit, 16-placeholder limit, 4,096-byte summary and rendered-subject limits, and rejection of bidi and invisible Unicode format characters.
The setting applies to the Review, Test, Document, Lint, and CI repair paths, not commits created by the Rebase or Push steps.

This non-executing field is read from the pushed branch, so a branch can adopt its own commit convention without enabling `allow_repo_commands`.

### intent

Override transcript-based user-intent extraction settings for this repo.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
| --- | --- | --- |
| `intent.enabled` | `bool` | Inherits from global (default `true`) |
| `intent.threshold` | `float` | Inherits from global (default `0.2`) |
| `intent.slack_days` | `int` | Inherits from global (default `3`) |
| `intent.disabled_readers` | `string[]` | Adds to globally disabled readers |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

### test.evidence

Configure repository publication of evidence artifacts from the test step.
Fields not set here inherit from global config and then the built-in defaults.

| Field | Type | Default |
| --- | --- | --- |
| `test.evidence.store_in_repo` | `bool` | Inherits from global (default `false`) |
| `test.evidence.attach_media` | `bool` | Inherits from global (default `true`) |
| `test.evidence.dir` | `string` | Inherits from global (default `.no-mistakes/evidence`) |
| `test.evidence.branch` | `string` | Inherits from global (default `no-mistakes/evidence`) |

By default, test evidence is written to `<NM_HOME>/evidence/<run-id>`. Where it is stored locally and how long it is kept are global-only settings; see [`test.evidence`](/no-mistakes/reference/global-config/#testevidence).
On GitHub.com/GHEC, supported image and video artifacts are uploaded to GitHub user-attachments when the PR is rendered unless `attach_media` is false and `store_in_repo` is also false.
For GitHub repositories, set `store_in_repo: true` to also publish it to an orphan evidence branch in the code branch's push-target repository and link the artifacts from the PR body; evidence is never committed to the pushed branch, so it never reaches the default branch.
`test.evidence.branch` is read ONLY from the trusted default-branch copy of this file, because it names a git ref the daemon pushes to; a pushed branch cannot redirect evidence commits.
See [global config](/no-mistakes/reference/global-config/#testevidence) for provider support, limits, validation, and fail-closed behavior.

### providers.github.draft_pull_requests

Override the [global GitHub draft setting](/no-mistakes/reference/global-config/#providersgithubdraft_pull_requests) for this repo.

| | |
|---|---|
| Type | `bool` |
| Default | Inherits from global (default `false`) |

### providers.gitlab.draft_pull_requests

Override the [global GitLab draft setting](/no-mistakes/reference/global-config/#providersgitlabdraft_pull_requests) for this repo.

| | |
|---|---|
| Type | `bool` |
| Default | Inherits from global (default `false`) |

### providers.bitbucket.draft_pull_requests

Override the [global Bitbucket draft setting](/no-mistakes/reference/global-config/#providersbitbucketdraft_pull_requests) for this repo.

| | |
|---|---|
| Type | `bool` |
| Default | Inherits from global (default `false`) |

### providers.azuredevops.draft_pull_requests

Override the [global Azure DevOps draft setting](/no-mistakes/reference/global-config/#providersazuredevopsdraft_pull_requests) for this repo.

| | |
|---|---|
| Type | `bool` |
| Default | Inherits from global (default `false`) |
