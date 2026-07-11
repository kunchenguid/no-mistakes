---
title: Configuration
description: Global and per-repo configuration options.
---

Configuration is optional. Without any config files, `no-mistakes` defaults to
`agent: auto`, which picks the first supported native agent available on your system,
with sensible defaults for everything else.

The goal is not to make you configure a mini CI system. The default path should
work. Config exists for the parts that genuinely vary by machine or repo:

- which agent or ordered fallback list you prefer
- which test or lint commands are the canonical ones for this repo
- where test evidence artifacts should be stored
- how aggressive the auto-fix loop should be
- how soon AXI should call an active step quiet
- whether the review loop reuses supported native agent sessions
- whether no-mistakes should infer intent from recent local agent transcripts

Config is split across two files:

| File                         | Scope                         | Full field reference                                          |
| ---------------------------- | ----------------------------- | ------------------------------------------------------------- |
| `~/.no-mistakes/config.yaml` | Global defaults for all repos | [Global Config Reference](/no-mistakes/reference/global-config/) |
| `<repo>/.no-mistakes.yaml`   | Per-repo overrides            | [Repo Config Reference](/no-mistakes/reference/repo-config/)     |

Set `NM_HOME` to relocate the global config directory (the global file becomes `$NM_HOME/config.yaml`).
Bitbucket Cloud credentials come from environment variables rather than config files.
For Azure DevOps, authenticate the `az` CLI with either `az devops login` or `AZURE_DEVOPS_EXT_PAT` for non-interactive daemon auth; see [Environment Variables](/no-mistakes/reference/environment/).

## How to think about config

- **Global config** is for your machine-level defaults.
- **Repo config** is for codebase-specific behavior that should travel with the repo.

In practice, most teams should keep personal preferences global and repo policy
local.

## What to configure first

If you are not sure where to start, configure these in this order:

1. Set `commands.test` and `commands.lint` in repo config so the gate runs the exact commands your repo expects.
2. Override `agent` per repo only when one codebase clearly works better with a different tool or fallback order.
3. Tune `auto_fix` after you have seen how much automation you actually want.

Everything else can usually wait.

The reference pages own each field's syntax, defaults, and exact semantics.
The rest of this page covers only the cross-cutting rules that involve both files at once.

## Precedence

- Repo config overrides global config field by field: repo `agent` replaces the global `agent` (including a full ordered fallback list), while `auto_fix`, `intent`, and `test.evidence` overlay individual fields and fall through to the global default for anything unset (`intent.disabled_readers` adds to the globally disabled readers instead of replacing them).
- `agent_path_override`, `agent_args_override`, `acpx_path`, `acp_registry_overrides`, `ci_timeout`, `daemon_connect_timeout`, `step_quiet_warning`, `log_level`, and `session_reuse` are global-only fields.
- `commands`, `ignore_patterns`, `document.instructions`, and `allow_repo_commands` are repo-only fields. By default, `commands` and `agent` are read from the trusted default branch; a trusted `allow_repo_commands: true` opt-in instead honors their pushed-branch values. `document.instructions` and `allow_repo_commands` themselves always come from the trusted default branch. See the [Repo Config Reference](/no-mistakes/reference/repo-config/) security note.
- no-mistakes reloads global config while setting up each run, so edits made before starting a run apply to it. For repeatable profiles (for example fast versus deep Codex settings), use separately initialized `NM_HOME` roots; `NM_HOME` moves all no-mistakes state, not just config.

## Explicit commands versus agent detection

Explicit `commands.test` and `commands.lint` give you deterministic baseline behavior, while leaving either empty asks the configured agent to fill the gap: empty `commands.test` has the agent detect and run tests, and empty `commands.lint` folds lint into the document step's combined housekeeping pass.
An empty `commands.format` runs no separate formatter, so configure it explicitly when the push step must format agent changes.
Either way, available user intent can trigger an evidence-oriented agent follow-up after a successful test baseline, and evidence stays in a temporary local directory unless the repo opts into `test.evidence.store_in_repo`.
The [Repo Config Reference](/no-mistakes/reference/repo-config/) owns the exact per-command semantics, including command process lifetime and the `ignore_patterns` match rules.

Before a new validation gate starts, its effective agent configuration must resolve to a runnable native agent or ACP bridge; otherwise the gate fails before its first pipeline step, even when explicit commands are configured.
Run `no-mistakes doctor` to check the global runner, and see [Choosing an Agent](/no-mistakes/guides/agents/) for how agent selection and fallback lists behave.
