---
title: Routing and agents
description: How no-mistakes routes every agent invocation and how an agent drives the pipeline.
---

`no-mistakes` routes every agent invocation through one global routing contract.
Each invocation starts from a registered semantic purpose, such as `initial_review` or `pr_composition`.
A route maps that purpose to a finite, ordered cascade of profiles.
A profile is an ordered list of provider candidates.
A candidate names a runner, a model, and a normalized effort.
A runner identifies the executable and its provider failure domain: `codex` maps to `openai` and `claude` maps to `anthropic`.
The normalized efforts are `low`, `medium`, `high`, and `xhigh`.

There is no single-agent selector, no fallback-agent list, no arbitrary agent-argument or path override, and no numeric per-step attempt limit.
Model selection and repair escalation are the routing contract's job.

## How an invocation is routed

Every invocation resolves through the same chain: purpose, then route, then profile, then ordered candidates, then runner.
The router selects exactly one requested route tier.
It tries that profile's candidates serially, in their declared order.
It constructs a fresh native process for each launched candidate and supplies that candidate's model and normalized effort.
If routing for a purpose is unavailable, the invocation fails closed with an error instead of guessing.

## Default profiles

The built-in contract defines six profiles.
Each pairs an OpenAI-family first candidate with an Anthropic backup at the same effort.

The exact profile table lives in the [routing reference](/no-mistakes/reference/routing/#default-profiles).

The defaults deliberately omit pro model selectors.
Custom profiles may use them.

## Default routes

Every registered purpose has a route.
A multi-tier route escalates left to right: a later profile is tried only after an earlier tier fails to resolve the work.

The exact purpose-to-route table lives in the [routing reference](/no-mistakes/reference/routing/#default-routes).

## Provider circuits

A provider circuit is scoped to one pipeline run, and every circuit starts closed.
Only a terminal classified operational failure opens a candidate's provider domain: quota, outage, overload, auth, or a missing executable.
The native adapter exhausts its own retries first, so a transient error never opens a circuit.
When a domain opens, the profile tries its backup candidate.
A later candidate in an open domain is not launched; it is persisted as skipped with that failure domain.
If no candidate remains available, the invocation fails closed.
Non-operational failures, such as malformed model output or a cancelled invocation, fail immediately: they neither open a circuit nor fail over.

## Repair lineages and fail-closed exhaustion

Blocking review findings, with severity `error` or `warning`, enter a routed repair cascade when a fix is authorized.
Each blocking finding is recorded as a lineage: a durable, run-wide identity that is independent of the model's display ID.
At each tier, the pipeline creates a fresh fixer, commits its patch, runs the applicable deterministic checks, then invokes a fresh verifier.
A lineage that finishes its finite cascade unresolved or inconclusive stays blocking.
Unattended mode aborts the run instead of approving or retrying it.
A normal gate stays parked for an explicit user decision.

## Durable routing observability

Before each native launch, no-mistakes appends an immutable, secret-free attempt record: the purpose, role, durable owner, and the candidate's profile, tier, index, runner, model, and effort.
When the attempt finishes, it appends at most one terminal fact: the outcome, the classified failure domain when there is one, the duration, and token counts.
The persisted history reconstructs escalation, provider failover, and circuit skips without re-reading current configuration.
`no-mistakes axi status` projects this history for the review step as a `review_routing` object with an attempts table and a lineages table.

## Configuring routing

Omit `routing` from `~/.no-mistakes/config.yaml` to use the built-in contract above.
A declared `routing` block is a complete replacement, not a patch: it must declare runners, profiles, and a non-empty route for every registered purpose, and it is validated before use.
A repository's `.no-mistakes.yaml` may only map a purpose to one existing global profile through `routes`.
It cannot define runners, profiles, candidates, or execution mechanics.
Repository routes are read from the trusted default branch, never from the pushed branch.
See [Configuration](/no-mistakes/guides/configuration/) for the full schema.

## Driving no-mistakes as an agent

The primary way to put a change through the gate from inside a coding agent is the `/no-mistakes` skill.
A skill-aware tool like Claude Code supports two invocation modes.
Use bare `/no-mistakes` to validate existing committed work.
Use `/no-mistakes <task>` to have the agent first do the task, commit only that task's changes on a feature branch, then run the ten-step pipeline (intent, rebase, review, test, document, lint, verify, push, PR, CI) with the task text as `--intent`.
In both modes, it resolves low-risk findings on its own and stops to relay anything that needs your decision.

`no-mistakes init` installs that skill at user level: `~/.claude/skills/no-mistakes/SKILL.md` for Claude Code and `~/.agents/skills/no-mistakes/SKILL.md` for other skill-aware tools.
One install makes the skill available in every repo, without committing tool-generated files to any repo.
If your home directory consolidates `.claude` and `.agents` with symlinks, `init` follows the links and keeps the skill reachable from both logical paths.
Re-run `no-mistakes init` after an upgrade to refresh that skill, including overwriting stale `SKILL.md` content from an older binary.
The skill drives `no-mistakes axi`, a non-interactive command surface that prints TOON to stdout and progress to stderr.

Agents can also call `no-mistakes axi` directly:

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi status
no-mistakes axi respond --action approve
no-mistakes axi logs --step review --full
no-mistakes axi abort
no-mistakes axi abort --run <id>
```

Before starting validation, agents should run the `no-mistakes axi` home view.
If it shows `active_run`, inspect that current-branch run with `no-mistakes axi status`.
If it is parked at a gate, drive it with `no-mistakes axi respond`.
Reattach an in-flight run by re-running `no-mistakes axi run` when it still matches your current `HEAD`.
If it shows `other_branch_active_run`, leave that run alone and start validation for the current branch with `no-mistakes axi run --intent "..."`.
Use `no-mistakes axi abort --run <id>` only when you need to cancel a specific active run by id from outside its worktree.

When an agent starts a new run, `--intent` is required and should describe what the user wanted to accomplish, not what files changed.
Agents should prefer a few complete sentences over a terse summary, capturing user decisions, tradeoffs, constraints, ruled-out approaches, and explicit requests that would not be obvious from the diff alone.
If the repo is on the default branch or has uncommitted changes, direct `axi run` returns a structured error with the command the agent should run instead of silently creating a branch or commit.

Approval gates are exposed as `gate:` objects with finding IDs, severities, files, actions, descriptions, and help commands for `no-mistakes axi respond`.
Review findings are parked for explicit consent; the review gate output flags this with a `note`.
Each finding's `action` sets what consent means:

- `auto-fix` findings may be sent to the pipeline with `--action fix`, which puts them into the routed repair cascade
- `no-op` findings are informational and are never repaired, even when selected
- `ask-user` findings require an explicit user decision unless `--yes` supplies standing consent

A fix applies only to the finding IDs the responder selects.
Findings left unselected park again for a decision at the next gate.
When an agent stops for `ask-user`, it should relay each finding's ID, file, and full description to the user before choosing `approve`, `fix`, or `skip`.
Resolving a finding always means responding with `no-mistakes axi respond --action fix`, which has the pipeline apply the fix and re-review it.
The agent must not edit the code itself while a run is active.

While a non-terminal run is parked at an `awaiting_approval` or `fix_review` gate, the run object includes `awaiting_agent: parked <duration>`.
Use that field in `axi status` output to tell in one read that the run is waiting for the driving agent to send `axi respond`.
It is observability only: it does not auto-resume the run, change gate resolution, or make `--yes` the default.
A long-running `axi run` or `axi respond` call is working, not stalled.
The run never advances past a gate on its own, so the agent must read every return, respond at each `gate:`, and loop until an `outcome:`.

With `--yes`, the user's explicit standing consent drives gates unattended.
At an awaiting-approval gate it selects every finding that has an ID for one pipeline fix round when any finding is actionable, approves a gate with only `no-op` findings or no selectable findings, and approves the resulting fix review rather than starting another fix cycle.
Before every unattended resolution, no-mistakes checks the run's blocking repair lineages.
If any blocking lineage ended its routed repair cascade unresolved or inconclusive, unattended driving sends `abort` and returns an error; it does not approve or retry the gate.
The TUI's yolo mode applies the same unattended consent and the same fail-closed abort.

`--yes` still stops at `checks-passed`, because a human must review and merge the PR.
When CI is green but the PR is still open, `axi run` and `axi respond` return `outcome: checks-passed` with a help line pointing at the PR instead of waiting for a human merge.
That is a successful agent stopping point: report that the PR is ready and ask the user to review and merge it.
If this PR later falls behind the default branch or hits a merge conflict, the CI monitor rebases onto the base, resolves it, and re-pushes the branch automatically - run no command and never hand-rebase.
Only when that monitor is no longer running (PR closed, run aborted, idle-timeout, or auto-fix exhausted) recover with `no-mistakes rerun`.
Successful outcomes also instruct the agent to summarize the run, and include a `fixes` table when the pipeline applied fixes, so the agent can acknowledge what it missed and the user can review each fix.

After a `failed` or `cancelled` outcome, address the reported problem, commit the correction on the same feature branch, then start a fresh validation with `no-mistakes axi run --intent "..."` or use the TUI rerun action.
Never abort or rerun an active run to bypass a gate; those are between-runs actions.
When you make an additional fix after a gate round has already produced fix commits, commit it on top of the existing branch and run `no-mistakes axi run --intent "..."` with the original user intent.
Never abort-and-restart, reset the branch, or open a new branch in a way that drops prior gate-fix commits.
A fresh run re-validates the branch's current state, so already-resolved findings do not re-surface.
`no-mistakes axi abort` is idempotent: no active run is a successful no-op.

## Wizard utility routing

Setup-wizard branch and commit suggestions are standalone routed utility invocations, not fabricated pipeline work.
The wizard creates a durable `wizard` utility scope and binds the `branch_commit_suggestion` purpose to it.
It uses the trusted global routing configuration, or the built-in default when none is configured.
It has a fresh provider-circuit set for its own session.
It never derives model selection from the checked-out feature branch's execution settings.
If routing is unavailable, the wizard fails closed with an error instead of guessing a model.

## The routing canary

`no-mistakes axi canary` is the required observability report for the routing cutover.
It reports whether the canary is dormant or activated, the frozen baseline and routed cohort counts and completeness, execution-only agent-bearing step-round medians, escalation and failover counts, and the target status.
Activation freezes the ten most recent completed pre-activation runs as the baseline.
The routed cohort then admits successful post-activation runs, in completion order, until it has ten members.
The comparison target is a 30% median execution-time reduction: the routed median at or below 70% of the baseline median.
The target is advisory only: it never changes profiles, routes, circuits, or gate outcomes.
Until both cohorts are complete, the report labels itself preliminary, and preliminary samples must not be treated as live results.
`target_met` stays `pending` until the cohorts are complete, so no live result is claimed before `routed_complete` reports ten successful routed gates.
