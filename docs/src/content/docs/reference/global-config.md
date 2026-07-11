---
title: Global config reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`.
Set `NM_HOME` to relocate the config directory.

Every field is optional.
Without a config file, no-mistakes uses the defaults shown below and the [built-in routing contract](/no-mistakes/reference/routing/).

```yaml
# ~/.no-mistakes/config.yaml

ci_timeout: "168h"

session_reuse: true

log_level: info

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence

# Omit 'routing' to use the built-in routing contract.
# A present routing block completely replaces the defaults and must be a
# full, valid contract. See the routing reference for the complete example.
```

Parsing is strict.
An unknown key is a load error, and a removed key fails with actionable guidance instead of being ignored or rewritten.
See [removed keys](#removed-keys) below.

## Fields

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and GitHub or GitLab mergeability, before giving up.

| | |
|---|---|
| Type | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days) |

Accepts any Go `time.ParseDuration` string, such as `30m`, `2h`, or `4h30m`.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor, and keeps getting rebased, no matter how long it stays open, while a genuinely idle or abandoned PR is still reaped after the timeout elapses.

Set it to `unlimited` (`none`, `off`, and `never` are accepted keywords), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

The former `babysit_timeout` key was removed and is not an alias; a config that still sets it fails to load.

### session_reuse

Whether the review loop reuses durable, provider-native sessions within one run.

| | |
|---|---|
| Type | `bool` |
| Default | `true` |

The initial review and every full rereview share one reviewer session.
Review-fix turns share a separate fixer session, so the roles never share context.
Claude resumes with `claude -p --resume <id>`, and Codex resumes with `codex exec resume <id> <prompt>`.
Other runners stay cold.

Session identities are scoped to one run and role.
No-mistakes stores only the run, role, provider, and native session ID so a parked run can restore them after a daemon restart.
A routed resume stays pinned to the provider that created the session.
If it fails, no-mistakes deletes the identity and journals a cold retry of the same purpose, route tier, and durable scope.
Set this field to `false` to make every review-loop invocation cold.

### log_level

Daemon log verbosity.

| | |
|---|---|
| Type | `string` |
| Values | `debug`, `info`, `warn`, `error` |
| Default | `info` |

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to the routed rebase, review, test, document, lint, CI repair, and PR invocations, and include it in generated PR descriptions.
These settings control transcript inference only.
An explicit `no-mistakes axi run --intent "..."` bypasses inference even when `intent.enabled` is `false`.
Downstream prompts treat explicit intent as authoritative acceptance criteria and inferred intent as a guarded hint.
If a review finds that the change contradicts explicit required or forbidden criteria, it emits an `ask-user` finding and parks for a decision.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `intent.enabled` | `bool` | `true` | Enable transcript-based intent extraction |
| `intent.threshold` | `float` | `0.2` | Minimum raw match score for selecting a transcript session |
| `intent.slack_days` | `int` | `3` | Extra days to look back before the change window |
| `intent.disabled_readers` | `string[]` | Empty | Transcript readers to disable |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated through a routed `intent_disambiguation` invocation.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts stay in a temporary directory keyed by run ID and are referenced by local path.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `test.evidence.store_in_repo` | `bool` | `false` | Commit and push test evidence artifacts from inside the repo worktree |
| `test.evidence.dir` | `string` | `.no-mistakes/evidence` | Repo-relative parent directory used when `store_in_repo` is true |

When `store_in_repo` is true, the test step writes, stages, and commits evidence under `<dir>/<branch-slug>` before the candidate is sealed.
Push transports that sealed candidate without staging or committing files.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.

These are global defaults.
Per-repo config can override either field.

### routing

The global model-selection contract: runners, profiles, and routes.

| | |
|---|---|
| Type | `object` |
| Default | Omitted (the built-in contract applies) |

Omit `routing` to use the built-in routing contract.
If `routing` is present, it is a complete replacement, not a patch:

- it must declare non-empty `runners`, `profiles`, and `routes`
- every registered purpose must have one non-empty route
- every route profile must exist in the same block and must not repeat within a route
- every candidate must reference a declared runner and provide a non-empty model and one normalized effort (`low`, `medium`, `high`, `xhigh`)
- every runner must provide a non-empty executable and its canonical failure domain (`codex` with `openai`, `claude` with `anthropic`)

A partial block, or a present-but-empty `routing:` key, is rejected at load, so a broken contract never reaches a model launch.

The [routing reference](/no-mistakes/reference/routing/) documents the default profile and route tables and includes a complete, valid custom routing example to copy and edit.

## Removed keys

Model selection is owned entirely by the routing contract.
The former agent-selection and per-step limit keys were removed in a clean cutover: there are no aliases, compatibility spellings, or automatic rewrites.

A config that still sets one of these keys fails to load with:

```
global config key "<key>" is no longer supported: <guidance>
```

The guidance column quotes the emitted guidance text exactly.

| Removed key | Guidance |
|---|---|
| `agent` | model selection is configured via `routing` (runners, profiles, routes); there is no single-agent selector |
| `fallback_agents` | provider fail-over is configured through routing profile candidates, not a fallback-agent list |
| `acpx_path` | acp agents were removed; declare runners under `routing.runners` |
| `acp_registry_overrides` | acp agents were removed; declare runners under `routing.runners` |
| `agent_path_override` | runner executables are configured via `routing.runners.<name>.executable` |
| `agent_args_override` | native agent arguments are derived from routing profile candidates and cannot be overridden |
| `auto_fix` | per-step numeric auto-fix limits were removed; repair escalates through the routing cascade |
| `babysit_timeout` | use `ci_timeout`; the `babysit_timeout` alias was removed |
| `daemon_connect_timeout` | daemon connection readiness is managed internally; this timeout is no longer configurable |
| `step_quiet_warning` | step liveness is reported from durable execution state; this warning threshold is no longer configurable |

See [migrating to routing](/no-mistakes/guides/migrating-to-routing/) for what to put in place of each key.

## Environment variables

See [environment variables](/no-mistakes/reference/environment/) for `NM_HOME`, Bitbucket Cloud credentials, and update-check suppression.
