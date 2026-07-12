---
title: Routing reference
description: The routing contract - runners, profiles, candidates, routes, purposes, provider circuits, repair lineages, and the canary report.
---

no-mistakes selects a model for every agent invocation through one global routing contract.
The contract is built in and needs no configuration.
This page is the canonical reference for the contract: its concepts, the exact default tables, custom routing, repository overrides, and the canary report.

For how a driving agent interacts with routed invocations, see the [routing and agents guide](/no-mistakes/guides/agents/).
For where routing lives in your config files, see the [global config reference](/no-mistakes/reference/global-config/#routing) and the [repo config reference](/no-mistakes/reference/repo-config/#routes).

## The contract in one pass

Every invocation resolves through the same chain: purpose, then route, then profile, then ordered candidates, then runner.

- a purpose is the registered semantic reason for an invocation, such as `initial_review` or `pr_composition`
- a route maps one purpose to a finite, ordered cascade of profiles
- a profile is an ordered list of provider candidates at one capability tier
- a candidate names one runner, one model, and one normalized effort
- a runner declares the executable and the provider failure domain it belongs to

There is no single-agent selector, no fallback-agent list, and no arbitrary argument, path, or model override outside this contract.
Configuration keys that used to provide those were removed without aliases.
See [migrating to routing](/no-mistakes/guides/migrating-to-routing/) if your config still has them.

### Purposes

The purpose registry is closed: only registered purposes can be routed, and every registered purpose must have a route.
Each purpose has a fixed role.
A fixer may produce a patch.
A verifier must independently judge existing work and never patches.

### Routes and tiers

A route is a finite, ordered escalation cascade of profile names.
Each position in the cascade is a tier.
Work starts at the tier its caller requests and escalates one tier at a time; a repair coordinator moves to the next tier only after the current tier fails to resolve the work.
A route may not repeat a profile, so every cascade terminates.
A requested tier outside the route fails closed instead of clamping to a weaker or stronger profile.

### Profiles and candidates

A profile groups provider candidates that share one capability intent.
Each profile declares its candidate order.
The built-in profiles put an OpenAI-family candidate first and an Anthropic backup second.
Candidates in a profile are tried serially, in declared order, and never raced.
A candidate's model is a free string, and its effort is one of the four normalized levels: `low`, `medium`, `high`, or `xhigh`.

### Runners

Only two runners exist: `codex` and `claude`.
Each runner declares a non-empty executable and its canonical provider failure domain: `codex` belongs to `openai` and `claude` belongs to `anthropic`.
A runner declaration with any other failure domain is rejected at load.
The runner translates the candidate's model and effort into native arguments; nothing else about the native command line is configurable:

| Runner | Model argument | Effort argument |
|---|---|---|
| `codex` | `-m <model>` | `-c model_reasoning_effort=<effort>` |
| `claude` | `--model <model>` | `--effort <effort>` |

Each launched candidate gets a fresh, steered native process with exactly the candidate's model and effort.

## Default profiles

The built-in contract defines six profiles.
Each pairs an OpenAI-family first candidate with an Anthropic backup at the same effort.

| Profile | Candidate 0 (preferred) | Candidate 1 (backup) |
|---|---|---|
| `fix_fast` | `codex` · `gpt-5.6-luna` · `medium` | `claude` · `claude-sonnet-5` · `medium` |
| `prose_fast` | `codex` · `gpt-5.6-luna` · `low` | `claude` · `claude-sonnet-5` · `low` |
| `fix_balanced` | `codex` · `gpt-5.6-terra` · `medium` | `claude` · `claude-opus-4-8` · `medium` |
| `tools_balanced` | `codex` · `gpt-5.6-terra` · `high` | `claude` · `claude-opus-4-8` · `high` |
| `review_strong` | `codex` · `gpt-5.6-sol` · `high` | `claude` · `claude-fable-5` · `high` |
| `authority_strong` | `codex` · `gpt-5.6-sol` · `xhigh` | `claude` · `claude-fable-5` · `xhigh` |

Pro model selectors, such as `gpt-5.6-sol-pro`, are valid but deliberately absent from the defaults.
Custom profiles may use them.

## Default routes

Every registered purpose has a default route.
A multi-tier route escalates left to right.

| Purpose | Role | Route |
|---|---|---|
| `initial_review` | verifier | `review_strong` |
| `structured_finding_repair` | fixer | `fix_fast` → `fix_balanced` → `authority_strong` |
| `intent_sensitive_repair` | fixer | `fix_balanced` → `authority_strong` |
| `unstructured_test_repair` | fixer | `fix_balanced` → `authority_strong` |
| `unstructured_ci_repair` | fixer | `fix_balanced` → `authority_strong` |
| `unstructured_conflict_repair` | fixer | `fix_balanced` → `authority_strong` |
| `test_evidence` | fixer | `tools_balanced` |
| `lint_inspection` | fixer | `tools_balanced` |
| `documentation_authoring` | fixer | `prose_fast` |
| `documentation_verification` | verifier | `tools_balanced` |
| `pr_composition` | fixer | `prose_fast` |
| `intent_summarization` | verifier | `prose_fast` |
| `intent_disambiguation` | verifier | `tools_balanced` |
| `branch_commit_suggestion` | fixer | `prose_fast` |
| `normal_aggregate_verification` | verifier | `review_strong` |
| `escalated_aggregate_verification` | verifier | `authority_strong` |
| `informational_repair` | fixer | `fix_fast` → `tools_balanced` |
| `informational_repair_verification` | verifier | `tools_balanced` |

## Provider circuits

A provider circuit tracks one failure domain (`openai` or `anthropic`) for the length of one pipeline run.
A standalone utility caller, such as the setup wizard, gets its own fresh circuit set for its session.
All circuits start closed.

Only a terminal classified operational failure opens a circuit: a quota, outage, overload, authentication, or missing-executable error that survives the native adapter's own retry loop.
When a launched candidate fails that way, its runner's canonical failure domain opens and the profile tries the next declared candidate.
Any later candidate whose domain is already open is not launched; it is persisted as skipped with that failure domain.
If no candidate in the profile remains available, the invocation fails closed rather than weakening the profile.

Non-operational failures stop immediately.
Malformed output, a schema violation, a cancelled context, or a genuinely bad result neither opens a circuit nor fails over; the caller sees the real cause.

## Immutable attempt history

Every candidate attempt is journalled durably before and after launch.

Before the native process starts, no-mistakes appends an immutable, secret-free start fact.
It records the purpose, role, durable pipeline or utility owner, profile, tier, candidate index, runner, model, effort, and candidate key.
After the attempt ends, it appends at most one terminal fact with the outcome, the classified failure domain when applicable, the duration, and token counters.

The persisted history reconstructs escalation, provider failover, and circuit skips without re-resolving the current config.
`no-mistakes axi status` and the review projections render from these facts.

## Repair lineages

A review finding that needs repair receives a stable, run-wide lineage identity that is independent of the model's display ID.
Repair escalates the finding through its purpose's route, one tier at a time.
At every tier the coordinator creates a fresh fixer, commits its patch, runs the applicable deterministic checks, and then invokes a fresh verifier.
Each tier persists the lineage, the immutable finding content, the tier and remaining budget, the linked fixer and verifier attempts, the deterministic checks, the verdict, and the terminal status.
When the cascade is exhausted without a clean verdict, the lineage is persisted as unresolved and the gate fails closed.
In unattended mode an unresolved blocking lineage aborts the run instead of triggering another fix or approval.

## Custom routing

You can replace the built-in contract with your own under `routing` in `~/.no-mistakes/config.yaml`.
Routing is global-only; a repository can never define runners, profiles, or candidates.

A present `routing` block is a complete replacement, not a patch.
It must declare:

- non-empty `runners`, `profiles`, and `routes`
- one route for every registered purpose, with no route empty and no profile repeated within a route
- only profiles that exist in the same block, referenced from routes
- candidates that reference a declared runner and carry a non-empty model and one normalized effort
- a non-empty executable and the canonical failure domain for each runner

A partial block is invalid and the config fails to load, so a broken contract never reaches a model launch.
A present-but-empty `routing:` key is also rejected; omit the key entirely to keep the defaults.

The supported customization strategy is to copy the complete block below and edit it.
This example is the full default contract with one change: the `authority_strong` preferred candidate uses a pro model selector.

```yaml
# ~/.no-mistakes/config.yaml
routing:
  runners:
    codex:
      executable: codex
      failure_domain: openai
    claude:
      executable: claude
      failure_domain: anthropic
  profiles:
    fix_fast:
      candidates:
        - runner: codex
          model: gpt-5.6-luna
          effort: medium
        - runner: claude
          model: claude-sonnet-5
          effort: medium
    prose_fast:
      candidates:
        - runner: codex
          model: gpt-5.6-luna
          effort: low
        - runner: claude
          model: claude-sonnet-5
          effort: low
    fix_balanced:
      candidates:
        - runner: codex
          model: gpt-5.6-terra
          effort: medium
        - runner: claude
          model: claude-opus-4-8
          effort: medium
    tools_balanced:
      candidates:
        - runner: codex
          model: gpt-5.6-terra
          effort: high
        - runner: claude
          model: claude-opus-4-8
          effort: high
    review_strong:
      candidates:
        - runner: codex
          model: gpt-5.6-sol
          effort: high
        - runner: claude
          model: claude-fable-5
          effort: high
    authority_strong:
      candidates:
        - runner: codex
          model: gpt-5.6-sol-pro
          effort: xhigh
        - runner: claude
          model: claude-fable-5
          effort: xhigh
  routes:
    initial_review: [review_strong]
    structured_finding_repair: [fix_fast, fix_balanced, authority_strong]
    intent_sensitive_repair: [fix_balanced, authority_strong]
    unstructured_test_repair: [fix_balanced, authority_strong]
    unstructured_ci_repair: [fix_balanced, authority_strong]
    unstructured_conflict_repair: [fix_balanced, authority_strong]
    test_evidence: [tools_balanced]
    lint_inspection: [tools_balanced]
    documentation_authoring: [prose_fast]
    documentation_verification: [tools_balanced]
    pr_composition: [prose_fast]
    intent_summarization: [prose_fast]
    intent_disambiguation: [tools_balanced]
    branch_commit_suggestion: [prose_fast]
    normal_aggregate_verification: [review_strong]
    escalated_aggregate_verification: [authority_strong]
    informational_repair: [fix_fast, tools_balanced]
    informational_repair_verification: [tools_balanced]
```

Run `no-mistakes doctor` after editing; it loads the config and reports any routing validation error.

## Repository route overrides

A repository may point a purpose at a different capability tier without defining any execution mechanics.
In `.no-mistakes.yaml`, `routes` maps a registered purpose to the name of one existing global profile:

```yaml
# .no-mistakes.yaml
routes:
  documentation_authoring: fix_balanced
```

The rules are strict:

- the profile must exist in the effective global contract, or the run fails closed before launch
- the override replaces that purpose's complete global cascade with a one-element route
- overrides are read only from the trusted default-branch copy of `.no-mistakes.yaml`, never from a pushed branch, and `allow_repo_commands` does not change that
- a repository cannot define `routing`, `runners`, `profiles`, or `candidates`

See the [repo config reference](/no-mistakes/reference/repo-config/#routes) for the field details.

## Canary

The routing canary is the required before-and-after report for the routing cutover.
Read it with:

```bash
no-mistakes axi canary
```

### How the cohorts are built

Activation happens once, at the end of the first clean routed gate: after all its steps complete and immediately before that run is accepted as completed.
It stores a routing fingerprint and freezes the baseline cohort: the ten most recent completed runs before activation, with their workload facts.
After activation, completed runs are admitted into the routed cohort once, in completion order, until it has ten members.
Each cohort is complete only when it holds ten runs.

### What is measured

The compared metric is execution-only: for each run, the summed wall-clock of the step rounds that actually launched an agent invocation.
Time parked at gates, waiting on CI, or spent in rounds without agent work does not count.
The report compares the exact median of that metric across each cohort, including half-millisecond medians for even cohorts.
It also reports each cohort's run count, completeness, and total semantic escalation and operational provider-failover counts.
The `baseline_workloads` and `routed_workloads` tables identify every cohort run and give its `execution_ms`, exact summed terminal `invocation_ms`, per-run escalation and failover counts, `changed_files`, `changed_lines`, and `initial_findings`.
`changed_files` and `changed_lines` are `-1` only when those git-derived facts were unavailable when the cohort member was frozen.
A semantic escalation is a transition from a launched attempt to a higher route tier within the same purpose and step round; merely starting at a higher tier does not invent an escalation.
An operational provider failover requires a classified failed terminal followed by a launched Candidate from another provider in the same profile and tier.
An already-open provider circuit's skipped Candidates remain durable evidence but never double-count the original failure or failover.

### The advisory target

The target is a 30% reduction in median execution time: the routed median at or below 70% of the baseline median.
The target is advisory only.
It never changes profiles, routes, circuits, or gate outcomes.

`target_met` stays `pending` until both ten-run cohorts are complete.
Until then the report's `result_state` is `preliminary` (or `dormant` before activation), and preliminary samples must not be treated as a live result.
Only a report with `comparison_complete: true` carries a `target_met` of `true` or `false`.
