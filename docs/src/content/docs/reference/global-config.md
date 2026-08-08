---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

acpx_path: acpx

forgejo_axi_path: forgejo-axi

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot

agent_args_override:
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"

ci_timeout: "168h"

step_quiet_warning: "10m"

daemon_connect_timeout: "3s"

log_level: info

session_reuse: true

auto_fix:
  rebase: 3
  review: 0
  test: 3
  document: 3
  lint: 3
  ci: 3

ci:
  rerun_transient: 0

commit:
  fix_message: "chore(no-mistakes-{{.Step}}): {{.Summary}}"

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence
    branch: no-mistakes/evidence
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

|         |                                                                                             |
| ------- | ------------------------------------------------------------------------------------------- |
| Type    | `string` or `string[]`                                                                      |
| Values  | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `cursor`, `acp:<target>` |
| Default | `auto`                                                                                      |

`auto` resolves to the first supported native agent or ACP alias in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, `copilot`, then `cursor`.
`cursor` is an ACP alias for the `cursor` target with default command `cursor-agent acp`.
With default paths, `auto` only selects it when both `cursor-agent` and `acpx` resolve; `acp_registry_overrides.cursor` and `acpx_path` replace those respective defaults during availability checks.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`; `acp:cursor` uses the same default command as `cursor`.
Arbitrary `acp:<target>` agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If an explicit agent is unavailable, `auto` finds no native agent or ACP alias, or no fallback-list entry is available, the gate fails before its first pipeline step rather than reporting a partial command-only validation as passed.
`no-mistakes doctor` checks the global configuration, while every run repeats resolution after applying any trusted repository-level `agent` override.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
After resolving `auto`, entries that resolve to the same ACP target are deduplicated in list order, so `cursor` and `acp:cursor` provide one fallback and preserve whichever spelling appears first.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>` and ACP aliases such as `agent: cursor`.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | `acpx`   |

### forgejo_axi_path

Executable used for Forgejo PR and CI operations.

|         |               |
| ------- | ------------- |
| Type    | `string`      |
| Default | `forgejo-axi` |

A bare name is resolved from the daemon's effective `PATH`; an explicit path is executed directly. See [Provider Integration](/no-mistakes/guides/provider-integration/#forgejo) for setup and the [environment reference](/no-mistakes/reference/environment/#forgejo_base_url) for host and token configuration.

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.
ACP aliases use the same target keys. For example, `agent: cursor` and `agent: acp:cursor` resolve to the `cursor` target, so set `cursor` to override the default `cursor-agent acp` command.
Values are trimmed; a blank or whitespace-only value behaves as no override, so an alias keeps its default command.
Availability checks always resolve `acpx_path`. They also probe the executable named first in the effective non-blank raw command when it is a bare command name or clean absolute path. Relative, quoted, or escaped raw commands are not pre-probed; `acpx` executes them from the worktree. These checks do not invoke the ACP target or test its credentials.

|         |                     |
| ------- | ------------------- |
| Type    | `map[string]string` |
| Default | Empty               |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents and aliases use `acpx_path` for the bridge; use `acp_registry_overrides` to replace a raw target command such as `cursor-agent acp`.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent      | Binary     |
| ---------- | ---------- |
| `claude`   | `claude`   |
| `codex`    | `codex`    |
| `rovodev`  | `acli`     |
| `opencode` | `opencode` |
| `pi`       | `pi`       |
| `copilot`  | `copilot`  |

### agent_args_override

Extra CLI flags to pass to each native agent.
Use this to set model selection, service tier, reasoning effort, permission mode, or any other flag the underlying agent supports.

|         |                                                           |
| ------- | --------------------------------------------------------- |
| Type    | `map[string][]string`                                     |
| Keys    | `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot` |
| Default | Empty (no extra flags)                                    |

User-supplied flags are normally inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. Security suppression selected by trusted [`disable_project_settings`](/no-mistakes/reference/repo-config/#disable_project_settings) may be placed first while preserving a compatible operator pin. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent      | Reserved flags                                                                                              |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `claude`   | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema`, `-r`, `--resume`, `--session-id`, `-c`, `--continue`, `--fork-session` |
| `codex`    | `exec`, `resume`, `--resume`, `--session`, `--session-id`, `--thread`, `--thread-id`, `--last`, `--json`, `--color` |
| `rovodev`  | `rovodev`, `serve`, `--disable-session-token`                                                               |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs`                                                             |
| `pi`       | `--mode`, `--no-session`                                                                                    |
| `copilot`  | `-p`, `--prompt`, `--output-format`, `--no-color`                                                          |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.
The Claude and Codex session-control forms are reserved so no-mistakes can keep review-loop conversations deterministic: review turns stay session-free while the fixer keeps its own isolated durable session.

Smart defaults:

- For `claude`, supplying `--permission-mode` (or `--dangerously-skip-permissions`) suppresses the default `--dangerously-skip-permissions`.
- For `codex`, supplying `--ask-for-approval`, `--sandbox`, or `--dangerously-bypass-approvals-and-sandbox` suppresses the default `--dangerously-bypass-approvals-and-sandbox`.

Permission and sandbox flags affect the underlying agent, but they do not disable no-mistakes' pipeline prompt steering.
Pipeline agents are still told to keep intentional writes inside the worktree and avoid mutating system state outside it.

Example:

```yaml
agent_args_override:
  claude:
    - --model
    - sonnet
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"
  rovodev:
    - --profile
    - work
  opencode:
    - --model
    - gpt-5
  pi:
    - --provider
    - google
```

For Codex, `service_tier` and `model_reasoning_effort` tune different things: `service_tier` selects the speed or priority lane, while `model_reasoning_effort` selects reasoning depth. no-mistakes reloads global config while setting up each run, so edits made before `no-mistakes axi run` apply to that run. For repeatable profiles, use separately initialized `NM_HOME` directories; each has its own `config.yaml` and no-mistakes state.

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and on GitHub, GitLab, Forgejo, or Azure DevOps PR mergeability, before giving up.

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open.
If it later develops an actual GitHub, GitLab, Forgejo, or Azure DevOps merge conflict, the CI auto-fix path rebases and re-pushes it, while a clean behind PR needs no command.
A genuinely idle/abandoned PR still parks at an approval gate after the timeout elapses.
While that CI gate is parked, the daemon continues bounded read-only PR-state checks.
If the PR is merged or closed externally, the stale gate completes automatically; an open, unknown, or temporarily unreachable PR remains parked for a user decision.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

Legacy alias: `babysit_timeout`.

### step_quiet_warning

How long a running or fixing step can go without recorded step-log or native-agent lifecycle activity before AXI status marks the step as quiet.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10m`                  |

Accepts any positive Go `time.ParseDuration` string: `30s`, `5m`, `1h`, etc.
Non-positive values are ignored and keep the default.

This is observability only.
It does not cancel the step, change auto-fix behavior, or mark the run failed.
AXI renders the quiet signal in the `active_steps` table as part of `last_activity`, for example `quiet 12m3s ago: codex started pid=4242`.
For older active runs that do not yet have activity rows, AXI falls back to the step log file's modification time.

### daemon_connect_timeout

Maximum time a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging. Guards against a daemon process that is alive but stuck or unresponsive.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `3s`                   |

Accepts any positive Go `time.ParseDuration` string. Overridable per-invocation with the `NM_DAEMON_CONNECT_TIMEOUT` environment variable; see [Environment Variables](/no-mistakes/reference/environment/#nm_daemon_connect_timeout).

### log_level

Daemon log verbosity.

|         |                                  |
| ------- | -------------------------------- |
| Type    | `string`                         |
| Values  | `debug`, `info`, `warn`, `error` |
| Default | `info`                           |

### session_reuse

Per-run agent session reuse for the review loop's fixer role.

|         |        |
| ------- | ------ |
| Type    | `bool` |
| Default | `true` |

When enabled and the pipeline agent supports native session resume (claude via `--resume`, codex via `exec resume`), each run keeps one durable fixer session across its review-fix turns.
Review turns - the initial full review and every full rereview - always run as fresh, session-free invocations regardless of this setting: a rereview certifies fixes that implement the previous review turn's findings, so it must never resume the session that prescribed them; cross-round review context travels only in the explicit sanitized round history.
The fixer session is never lent to review turns, other pipeline steps stay session-isolated in their own cold invocations, and different runs never reuse identities.
When resume is unavailable or fails, the fix turn falls back to a cold run or a fresh fixer session and the fallback is recorded in the local `agent_invocations` performance record.
Session identities are persisted only as minimum local resume metadata, never as prompts or transcripts.
The [daemon crash-recovery reference](/no-mistakes/concepts/daemon/#crash-recovery) owns which parked gates can resume or reconcile after a restart.
Set `false` to force every agent invocation cold.

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings then pause for approval instead of starting another automatic fix loop.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field               | Type  | Default | Description                                                                                 |
| ------------------- | ----- | ------- | ------------------------------------------------------------------------------------------- |
| `auto_fix.rebase`   | `int` | `3`     | Rebase conflict auto-fix attempts                                                           |
| `auto_fix.review`   | `int` | `0`     | Review finding auto-fix attempts                                                            |
| `auto_fix.test`     | `int` | `3`     | Test failure auto-fix attempts                                                              |
| `auto_fix.document` | `int` | `3`     | Not used by the automatic document pass                                                     |
| `auto_fix.lint`     | `int` | `3`     | Lint issue auto-fix attempts                                                                |
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus GitHub, GitLab, Forgejo, and Azure DevOps merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

### ci.rerun_transient

How many times the CI step may re-run a single check the provider reported as cancelled before that check reaches an approval gate.

| | |
|---|---|
| Type | `int` |
| Default | `0` |
| Range | `0` to `5`; values outside it are clamped |

```yaml
ci:
  rerun_transient: 0
```

Each rerun is another provider-side workflow run billed to the repository being contributed to.
Set `0` here to never spend someone else's CI minutes; this is the only place to make that choice for a repository whose default branch you do not control.

The per-repo [`ci.rerun_transient`](/no-mistakes/reference/repo-config/#cirerun_transient) overrides this value and owns the classification, the trust boundary, and every case that skips the rerun.

### commit.fix_message

Template for the subject of commits created by the shared Review, Test, Document, and Lint fix path.

| | |
| --- | --- |
| Type | `string` |
| Default | `no-mistakes({{.Step}}): {{.Summary}}` |

The template supports literal text and two Go-style placeholders:

| Variable | Value |
| --- | --- |
| `{{.Step}}` | Pipeline step name, such as `review`, `test`, `document`, or `lint` |
| `{{.Summary}}` | Sanitized one-line summary returned by the fix agent, or the step's deterministic fallback summary |

The value must be a valid UTF-8 template that renders to a non-empty, single-line commit subject.
The template source is limited to 1,024 bytes and 16 placeholders.
The fix-agent summary and final rendered subject are each limited to 4,096 bytes.
Before rendering, no-mistakes predicts the subject size from the validated literal text and placeholders, then rejects oversized output without allocating the expanded message.
Template functions, control actions, named templates, unknown placeholders, malformed syntax, control characters, unsafe Unicode format characters, and Unicode line or paragraph separators cause configuration loading to fail.
The blocked format set includes every Unicode `Bidi_Control` code point plus `U+00AD`, `U+180E`, `U+200B`, `U+2060` through `U+2064`, the deprecated bidi controls `U+206A` through `U+206F`, `U+FEFF`, `U+FFF9` through `U+FFFB`, and Unicode tag characters in `U+E0000` through `U+E007F`.
Legitimate `U+200C` zero-width non-joiner and `U+200D` zero-width joiner text shaping remains allowed.
The final rendered subject is validated again, so unsafe characters in an agent-provided summary are also rejected.
The setting does not change commit subjects created by the Rebase, CI, or Push steps.
A per-repo [`commit.fix_message`](/no-mistakes/reference/repo-config/#commitfix_message) value overrides this global setting.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to rebase, review, test, document, lint, CI auto-fix, and PR prompts, and include it in generated PR descriptions.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts stay in a temporary directory keyed by run ID and are referenced by local path.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                         | Type     | Default                 | Description                                                                 |
| ----------------------------- | -------- | ----------------------- | --------------------------------------------------------------------------- |
| `test.evidence.store_in_repo` | `bool`   | `false`                 | Publish test evidence artifacts to the repository's orphan evidence branch  |
| `test.evidence.dir`           | `string` | `.no-mistakes/evidence` | Directory prefix inside the evidence branch                                 |
| `test.evidence.branch`        | `string` | `no-mistakes/evidence`  | Name of the orphan evidence branch                                          |

The test step always collects evidence in a temporary directory outside the worktree, so artifacts never enter the branch under validation.
When `store_in_repo` is true for a GitHub repository, the PR step copies that directory onto `branch` under `<dir>/<branch-slug>` in the code branch's push-target repository (the fork when fork routing is configured), pushes it, and links the artifacts from the pull request body.
The branch is an orphan: it shares no history with your code branches, so evidence never reaches the default branch. Links use the evidence commit rather than the branch, so they keep resolving after later runs.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
`branch` must be a valid Git branch name; an invalid value fails the config with the offending key and value.
The publisher never force-pushes. It appends to the fetched evidence-branch tip with a fast-forward push, retries one lost race, and refuses to use the run branch, default branch, or an existing branch whose tip lacks the `.no-mistakes-evidence` marker.
Publication is also refused when the remote cannot be read or pushed, an artifact exceeds 64 MiB, a run exceeds 500 files or 256 MiB, or another writer wins the retry. The PR body then keeps its local rendering instead of adding links that would not resolve.
Evidence-branch publication currently supports GitHub links only. On other providers, no evidence branch is pushed and the PR body keeps its local rendering.
Enabling this pushes a branch to your remote, so pick a `branch` name your CI workflows do not build.

These are global defaults. Per-repo config can override each field, except `branch`, which is read only from the trusted default branch.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, Forgejo host and token settings, Bitbucket Cloud credentials, and update-check suppression.
