---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode

ci_timeout: "4h"

log_level: info

auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 3
  ci: 3
```

## Fields

### agent

Default agent for all repos. Can be overridden per-repo.

| | |
|---|---|
| Type | `string` |
| Values | `auto`, `claude`, `codex`, `rovodev`, `opencode` |
| Default | `auto` |

`auto` resolves to the first supported agent found on `PATH` in this order: `claude`, `codex`, `opencode`, then `acli` with `rovodev` support.

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

### ci_timeout

How long the babysit step waits for CI and PR mergeability before timing out.

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
| `auto_fix.review` | `int` | `3` | Review finding auto-fix attempts |
| `auto_fix.test` | `int` | `3` | Test failure auto-fix attempts |
| `auto_fix.document` | `int` | `3` | Documentation update auto-fix attempts |
| `auto_fix.lint` | `int` | `3` | Lint issue auto-fix attempts |
| `auto_fix.ci` | `int` | `3` | CI/Babysit auto-fix attempts for CI failures and merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

## Environment variables

| Variable | Description |
|---|---|
| `NM_HOME` | Override the data directory (default `~/.no-mistakes/`) |
| `NO_MISTAKES_NO_UPDATE_CHECK` | Set to `1` to suppress background update checks |
