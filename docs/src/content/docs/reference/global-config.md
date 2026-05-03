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
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

| | |
|---|---|
| Type | `string` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `acp:<target>` |
| Default | `auto` |

`auto` resolves to the first supported agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, then `pi`.
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

Custom binary paths for each agent. When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.

| | |
|---|---|
| Type | `map[string]string` |
| Default | Empty (uses default binary names) |

Default binary names when no override is set:

| Agent | Binary |
|---|---|
| `claude` | `claude` |
| `codex` | `codex` |
| `rovodev` | `acli` |
| `opencode` | `opencode` |
| `pi` | `pi` |

### agent_args_override

Extra CLI flags to pass to each agent. Use this to set model selection, reasoning effort, permission mode, or any other flag the underlying agent supports.

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

How long the CI step waits for provider CI status, and on GitHub or GitLab for PR mergeability, before timing out.

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

Maximum auto-fix attempts per step. Set a step to `0` to disable auto-fix (findings always require manual approval).

| | |
|---|---|
| Type | `object` |

| Field | Type | Default | Description |
|---|---|---|---|
| `auto_fix.rebase` | `int` | `3` | Rebase conflict auto-fix attempts |
| `auto_fix.review` | `int` | `0` | Review finding auto-fix attempts |
| `auto_fix.test` | `int` | `3` | Test failure auto-fix attempts |
| `auto_fix.document` | `int` | `3` | Documentation update auto-fix attempts |
| `auto_fix.lint` | `int` | `3` | Lint issue auto-fix attempts |
| `auto_fix.ci` | `int` | `3` | CI auto-fix attempts for CI failures, plus GitHub and GitLab merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, Bitbucket Cloud credentials, and update-check suppression.
