---
title: Routing and agents
description: How no-mistakes routes agent work and where agents find current instructions.
---

`no-mistakes` routes every agent invocation through one global routing contract.
This guide explains the stable rules that affect routing.
See the [routing reference](/no-mistakes/reference/routing/) for the exact default profiles, routes and configuration.

## The routing contract

Every invocation has a registered semantic purpose, such as `initial_review` or `pr_composition`.
A route maps that purpose to a finite, ordered cascade of profiles.
A profile contains an ordered list of provider candidates.
A candidate names a runner, a model and a normalized effort.
A runner identifies its executable and provider failure domain.
The only runners are `codex` and `claude`.
The normalized efforts are `low`, `medium`, `high` and `xhigh`.

There is no single-agent selector or fallback-agent list.
There is no arbitrary agent argument, path or model override outside the routing contract.
There is no numeric attempt limit for a pipeline step.
Model selection, provider failover and repair escalation belong to the routing contract.

## How routing works

The routing chain is purpose, route, profile, ordered candidate and runner.
The router selects one requested route tier and tries its candidates in order.
Each launched candidate gets a fresh native process with its declared model and effort.
An unavailable purpose or invalid tier fails closed instead of selecting another route.

The built-in profiles prefer an OpenAI candidate and use an Anthropic candidate as backup.
See the [default routing tables](/no-mistakes/reference/routing/#default-profiles) for the current candidates and purpose mappings.

## Provider circuits

Provider circuits last for one pipeline run and start closed.
A terminal operational failure can open a provider domain after the native adapter has exhausted its retries.
Operational failures include quota, outage, overload, authentication and a missing executable.
The profile then tries the next candidate in another available provider domain.
A candidate in an open domain is recorded as skipped instead of being launched.
The invocation fails closed when no candidate remains.

Malformed output, schema errors and cancellation do not open a circuit.
These failures stop the invocation without provider failover.

## Repair lineages

Each blocking review finding has a durable identity across the run.
An authorized repair moves through its finite route one tier at a time.
Each tier uses a fresh fixer and a fresh verifier.
The verifier judges the existing candidate independently and does not patch it.
The pipeline records the repair, deterministic checks and verifier result for each tier.

An unresolved or inconclusive finding stays blocking after its route is exhausted.
Unattended mode aborts instead of approving or retrying that finding.
A normal gate stays parked for an explicit decision.

## Review session reuse

`session_reuse` defaults to `true` and applies only to the review loop.
A run keeps one reviewer session for the initial review and full rereviews.
It keeps a separate fixer session for review fixes.
Sessions never cross runs, roles, branches or repositories.

A resumed session stays with the provider and Review role that created it.
If resume fails, no-mistakes removes that stored identity and retries the same routed turn cold.
A cancelled turn does not start a cold fallback.
No-mistakes stores native session IDs but does not store prompts or transcripts.

## Routing history

No-mistakes records each native launch before it starts.
The record identifies the purpose, role, durable owner, profile, tier, candidate, runner, model and effort.
The terminal record adds the outcome, failure domain, duration and token counts.
This immutable history reconstructs escalation, provider failover and circuit skips without reading current configuration.
`no-mistakes axi status` shows the review routing attempts and repair lineages for the run.

## Configure routing

Omit `routing` from `~/.no-mistakes/config.yaml` to use the built-in contract.
A declared `routing` block replaces the whole contract.
It must define every runner, profile and registered purpose before any agent can launch.

A repository can map a purpose to one existing global profile through trusted `routes` configuration.
It cannot define runners, profiles, candidates or execution mechanics.
By default, repository commands come from the pinned trusted default branch.
`allow_repo_commands` can opt pushed-branch commands in when the trusted default branch enables it.
Repository `routes` and `document.instructions` always remain trusted-only.
See the [configuration guide](/no-mistakes/guides/configuration/) for the supported files and trust rules.

## Driving no-mistakes as an agent

`no-mistakes init` installs the generated [installable `/no-mistakes` skill](https://github.com/kunchenguid/no-mistakes/blob/main/skills/no-mistakes/SKILL.md) at user level.
It installs the skill at `~/.claude/skills/no-mistakes/SKILL.md` and `~/.agents/skills/no-mistakes/SKILL.md`.
Re-run `no-mistakes init` after an upgrade to refresh the installed skill.

The installed skill owns the current workflow for starting, resuming and responding to a run.
Live AXI output owns the next action at each gate and outcome.
Run `no-mistakes axi run --help` for the current options.
The [AXI command reference](/no-mistakes/reference/cli/#no-mistakes-axi) explains the command surface.

Only these recovery invariants are repeated here because getting them wrong can discard validated work.
If a monitored pull request falls behind or conflicts after `checks-passed`, the monitor resolves the conflict and re-pushes the branch.
Run no command and never hand-rebase while that monitor is active.
Use `no-mistakes rerun` only after the monitor has stopped.

After a failed or cancelled run, commit the correction on the same feature branch and start fresh validation with `no-mistakes axi run --intent`.
Never abort-and-restart, reset the branch or create another branch in a way that discards prior gate-fix commits.
A fresh run validates the branch's current state, so already-resolved findings do not re-surface.

## Wizard utility routing

Setup-wizard branch and commit suggestions are standalone routed utility invocations.
The wizard uses the `branch_commit_suggestion` purpose and a durable `wizard` scope.
It uses trusted global routing and gets a fresh provider-circuit set for its session.
It fails closed when routing is unavailable.
See the [setup wizard guide](/no-mistakes/guides/setup-wizard/) for the user workflow.

## Routing canary

`no-mistakes axi canary` reports on the routing cutover.
It compares a frozen pre-activation baseline with a routed cohort using execution-only, agent-bearing measurements.
Its target is advisory and never changes routes, profiles, circuits or gate outcomes.
Results stay preliminary until both cohorts are complete.
See the [routing canary reference](/no-mistakes/reference/routing/#canary) for the current cohort and target rules.
