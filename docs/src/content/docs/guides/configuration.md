---
title: Configuration
description: Global and per-repo configuration options.
---

Configuration is optional. Without any config files, `no-mistakes` defaults to
`agent: auto`, which picks the first supported agent available on your system,
with sensible defaults for everything else.

The goal is not to make you configure a mini CI system. The default path should
work. Config exists for the parts that genuinely vary by machine or repo:

- which agent you prefer
- which test or lint commands are the canonical ones for this repo
- how aggressive the auto-fix loop should be

Config is split across two files:

| File | Scope |
|---|---|
| `~/.no-mistakes/config.yaml` | Global defaults for all repos |
| `<repo>/.no-mistakes.yaml` | Per-repo overrides |

Set `NM_HOME` to relocate the global config directory (the global file becomes `$NM_HOME/config.yaml`).

## How to think about config

- **Global config** is for your machine-level defaults.
- **Repo config** is for codebase-specific behavior that should travel with the repo.

In practice, most teams should keep personal preferences global and repo policy
local.

## What to configure first

If you are not sure where to start, configure these in this order:

1. Set `commands.test` and `commands.lint` in repo config so the gate runs the exact commands your repo expects.
2. Override `agent` per repo only when one codebase clearly works better with a different tool.
3. Tune `auto_fix` after you have seen how much automation you actually want.

Everything else can usually wait.

## Global config

```yaml
# ~/.no-mistakes/config.yaml

# Default agent for all repos and setup-wizard suggestions.
# "auto" picks the first available agent on PATH.
agent: auto  # auto | claude | codex | rovodev | opencode

# Optional binary path overrides.
agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode

# How long the CI step waits for provider CI status, and GitHub/GitLab PR mergeability, before timing out.
ci_timeout: "4h"  # any Go duration string

# Daemon log verbosity.
log_level: info  # debug | info | warn | error

# Max auto-fix attempts per step. 0 = disabled (requires manual approval).
auto_fix:
  rebase: 3
  document: 3
  lint: 3
  test: 3
  review: 0
  ci: 3
```

See [Global Config Reference](/no-mistakes/reference/global-config/) for the full field listing.

## Environment variables

Bitbucket Cloud PR creation and CI monitoring use environment variables instead of a provider CLI:

- `NO_MISTAKES_BITBUCKET_EMAIL`
- `NO_MISTAKES_BITBUCKET_API_TOKEN`
- `NO_MISTAKES_BITBUCKET_API_BASE_URL` - optional API base URL override

## Repo config

```yaml
# .no-mistakes.yaml (in repo root)

# Override the agent for this repo and its setup-wizard suggestions.
agent: codex

# Explicit commands for test/lint/format steps.
commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

# Ignore these paths during review and documentation checks.
ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Override auto-fix limits for this repo.
auto_fix:
  document: 3
  lint: 5
```

See [Repo Config Reference](/no-mistakes/reference/repo-config/) for the full field listing.

## Precedence

- Repo `agent` overrides global `agent`.
- Global `agent: auto` resolves by checking `claude`, `codex`, `opencode`, then `acli` for `rovodev` on `PATH`.
- `auto_fix` from the repo config overlays global auto_fix. Fields not set in the repo config fall through to the global default.
- `commands` and `ignore_patterns` are repo-only fields.
- `ci_timeout` and `auto_fix.ci` are the canonical keys; `babysit_timeout` and `auto_fix.babysit` are still accepted as legacy aliases.
- If `commands.test` or `commands.lint` is empty, the agent detects and runs relevant commands itself.
- If `commands.format` is empty, no formatter is run automatically.

The practical implication is simple: explicit commands give you deterministic
repo behavior, while leaving commands empty asks the agent to fill in the gap.

## Ignore pattern rules

Patterns in `ignore_patterns` control which files are excluded from review and documentation checks:

| Pattern | Match rule |
|---|---|
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire directory subtree |
| `some/path/file.go` | Contains a slash - full path glob matching |
