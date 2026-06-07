---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

acpx_path: acpx

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi

agent_args_override:
  codex:
    - -m
    - gpt-5.4
    - --full-auto

ci_timeout: "4h"

log_level: info

auto_fix:
  rebase: 3
  review: 0
  test: 3
  document: 3
  lint: 3
  ci: 3

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

| | |
|---|---|
| Type | `string` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `acp:<target>` |
| Default | `auto` |

`auto` resolves to the first supported native agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, then `pi`.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`.
ACP agents are opt-in and are not considered by `agent: auto`.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>`.

| | |
|---|---|
| Type | `string` |
| Default | `acpx` |

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.

| | |
|---|---|
| Type | `map[string]string` |
| Default | Empty |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents use `acpx_path` instead.

| | |
|---|---|
| Type | `map[string]string` |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent | Binary |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `rovodev` | `acli` |
| `opencode` | `opencode` |
| `pi` | `pi` |

### agent_args_override

Extra CLI flags to pass to each native agent.
Use this to set model selection, reasoning effort, permission mode, or any other flag the underlying agent supports.

| | |
|---|---|
| Type | `map[string][]string` |
| Keys | `claude`, `codex`, `rovodev`, `opencode`, `pi` |
| Default | Empty (no extra flags) |

User-supplied flags are inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent | Reserved flags |
|---|---|
| `claude` | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema` |
| `codex` | `exec`, `--json`, `--color` |
| `rovodev` | `rovodev`, `serve`, `--disable-session-token` |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs` |
| `pi` | `--mode`, `--no-session` |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.

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
    - --full-auto
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

### ci_timeout

How long the CI step monitors an open PR, including provider CI status and on GitHub or GitLab PR mergeability, before timing out.

| | |
|---|---|
| Type | `string` (Go duration) |
| Default | `4h` |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

Legacy alias: `babysit_timeout`.

### log_level

Daemon log verbosity.

| | |
|---|---|
| Type | `string` |
| Values | `debug`, `info`, `warn`, `error` |
| Default | `info` |

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the agent still attempts safe fixes during the initial lint pass; unresolved lint findings then pause for approval instead of starting another automatic fix loop.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `auto_fix.rebase` | `int` | `3` | Rebase conflict auto-fix attempts |
| `auto_fix.review` | `int` | `0` | Review finding auto-fix attempts |
| `auto_fix.test` | `int` | `3` | Test failure auto-fix attempts |
| `auto_fix.document` | `int` | `3` | Not used by the automatic document pass |
| `auto_fix.lint` | `int` | `3` | Lint issue auto-fix attempts |
| `auto_fix.ci` | `int` | `3` | CI auto-fix attempts for CI failures, plus GitHub and GitLab merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

### intent

User-intent extraction settings.
When enabled, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to rebase, review, test, document, lint, CI auto-fix, and PR prompts, and include it in generated PR descriptions.

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `intent.enabled` | `bool` | `true` | Enable transcript-based intent extraction |
| `intent.threshold` | `float` | `0.2` | Minimum raw match score for selecting a transcript session |
| `intent.slack_days` | `int` | `3` | Extra days to look back before the change window |
| `intent.disabled_readers` | `string[]` | Empty | Transcript readers to disable |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, and `pi`.

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

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `test.evidence.store_in_repo` | `bool` | `false` | Commit and push test evidence artifacts from inside the repo worktree |
| `test.evidence.dir` | `string` | `.no-mistakes/evidence` | Repo-relative parent directory used when `store_in_repo` is true |

When `store_in_repo` is true, the test step writes evidence under `<dir>/<branch-slug>` and the push step stages files from that directory before committing agent changes.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.

These are global defaults. Per-repo config can override either field.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, Bitbucket Cloud credentials, and update-check suppression.
