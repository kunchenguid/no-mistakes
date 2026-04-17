---
title: Configuration
description: Global and per-repo configuration options.
---

Configuration is optional. Without any config files, `no-mistakes` defaults to `agent: auto`, which picks the first supported agent available on your system, with sensible defaults for everything else.

Config is split across two files:

| File | Scope |
|---|---|
| `~/.no-mistakes/config.yaml` | Global defaults for all repos |
| `<repo>/.no-mistakes.yaml` | Per-repo overrides |

Set `NM_HOME` to relocate the global config directory (the global file becomes `$NM_HOME/config.yaml`).

## Global config

```yaml
# ~/.no-mistakes/config.yaml

# Default agent for all repos.
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

# Override the agent for this repo only.
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

## Ignore pattern rules

Patterns in `ignore_patterns` control which files are excluded from review and documentation checks:

| Pattern | Match rule |
|---|---|
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire directory subtree |
| `some/path/file.go` | Contains a slash - full path glob matching |
