---
title: Configuration
description: Global and per-repo configuration options.
---

Configuration is optional.
Without any config files, no-mistakes selects models through the [built-in routing contract](/no-mistakes/reference/routing/), with sensible defaults for everything else.

The goal is not to make you configure a mini CI system.
The default path should work.
Config exists for the parts that genuinely vary by machine or repo:

- which test or lint commands are the canonical ones for this repo
- where test evidence artifacts should be stored
- whether no-mistakes should infer intent from recent local agent transcripts
- how long the CI monitor babysits an open PR
- which capability profile a specific purpose should use, when the defaults do not fit

Config is split across two files:

| File | Scope |
|---|---|
| `~/.no-mistakes/config.yaml` | Global defaults for all repos |
| `<repo>/.no-mistakes.yaml` | Per-repo overrides |

Set `NM_HOME` to relocate the global config directory (the global file becomes `$NM_HOME/config.yaml`).

## How to think about config

- Global config is for your machine-level defaults and, if you need one, a custom routing contract.
- Repo config is for codebase-specific behavior that should travel with the repo.

Model selection is deliberately not a per-repo choice.
The global routing contract owns runners, profiles, and candidates; a repository can only point a purpose at an existing global profile.

By default, no-mistakes loads `commands.test`, `commands.lint`, and `commands.format` from the pinned trusted default branch, not from the pushed SHA.
A trusted default-branch config can set `allow_repo_commands: true` to honor commands from pushed branches.
Use this exception only when you trust every pushed branch, such as in a single-developer repository.
`routes` and `document.instructions` always remain trusted-only, even when this exception is enabled.

If your config predates the routing cutover, read [migrating to routing](/no-mistakes/guides/migrating-to-routing/) first.
Removed keys such as `agent` or `auto_fix` now stop the config from loading.

## What to configure first

If you are not sure where to start, configure these in this order:

1. Set `commands.test` and `commands.lint` in repo config so the gate runs the exact commands your repo expects.
2. Opt into `test.evidence.store_in_repo` if your team wants evidence artifacts committed and linked from PRs.
3. Add a repo `routes` override only when one purpose clearly needs a different capability tier in this codebase.

Everything else can usually wait.

## Global config

```yaml
# ~/.no-mistakes/config.yaml

# How long the CI step monitors an open PR (provider CI status plus GitHub/GitLab
# mergeability) with no base-branch movement before giving up. Each base-branch
# advance re-arms the timer, so an actively-updated green PR keeps its monitor.
# Use "unlimited" (or "none", "off", "never", or any non-positive duration) to
# monitor until the PR is merged, closed, or aborted.
ci_timeout: "168h"

# Daemon log verbosity.
log_level: info  # debug | info | warn | error

# Infer the author's intent from recent local agent transcripts when not supplied directly.
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

# Test evidence defaults to temporary local storage.
test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence

# Model selection: omit 'routing' to use the built-in routing contract.
# A present routing block completely replaces the defaults: it must route
# every registered purpose and declare every profile and runner it
# references. Copy the full example from the routing reference and edit
# it; a block that misses any of those is rejected.
```

See the [global config reference](/no-mistakes/reference/global-config/) for the full field listing.

## Repo config

```yaml
# .no-mistakes.yaml (in repo root)


# Keep commands on the trusted default branch by default.
# Set true only when the trusted default branch opts into commands from every pushed branch.
allow_repo_commands: false
# Explicit commands for test/lint/format steps.
commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."

# Ignore these paths during review and documentation checks.
ignore_patterns:
  - "*.generated.go"
  - "vendor/**"

# Optional repo-level overrides for transcript-based intent extraction.
intent:
  enabled: true

# Opt in when evidence artifacts should be committed and linked from the PR.
test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence

# Optional: point a purpose at one existing global profile.
routes:
  documentation_authoring: fix_balanced
```

See the [repo config reference](/no-mistakes/reference/repo-config/) for the full field listing, including the trust boundary for `commands` and `routes`.

## Precedence

- Model selection resolves in one direction: the built-in routing contract applies unless global config declares a complete replacement `routing` block, and trusted repo `routes` overrides are applied on top of the effective global contract.
- A repo `routes` entry replaces that purpose's complete global cascade with a one-element route naming one existing global profile.
- `routes` and `document.instructions` always come from the trusted default-branch copy of `.no-mistakes.yaml`, never from the pushed branch.
- `commands` also come from the trusted default branch unless that trusted copy sets `allow_repo_commands: true`.
- The `allow_repo_commands` exception affects only commands, so routes and documentation ownership remain trusted-only.
- The resolved routing contract is validated before any model launch and fails closed on any error.
- `intent` from the repo config overlays global intent settings. Fields not set in the repo config fall through to the global default, except `intent.disabled_readers`, which adds to globally disabled readers.
- `test.evidence` from the repo config overlays global test evidence settings. Fields not set in the repo config fall through to the global default.
- `commands` and `ignore_patterns` are repo-only fields.
- `ci_timeout` and `log_level` are global-only fields.
- If `commands.test` is set, the test step runs it first as the baseline; when user intent is available, the routed agent may still run afterward to gather evidence-oriented validation.
- If `commands.test` is empty, the routed agent detects and runs relevant tests itself.
- If `commands.lint` is empty, the routed agent detects relevant linters and formatters, applies safe fixes, verifies them, commits any agent changes, and reports only unresolved issues.
- If `commands.format` is empty, no separate push-step formatter is run automatically.

The practical implication is simple: explicit commands give you deterministic baseline behavior, while leaving commands empty asks the routed agent to fill in the gap.
For tests, available user intent can also trigger an evidence-oriented agent follow-up after the baseline command succeeds.
By default, evidence stays in a temporary local directory; opt into `test.evidence.store_in_repo` when your team wants evidence artifacts committed, pushed, and linked directly from PRs.
For lint, that gap includes safe formatter and linter fixes during the initial lint pass.

## Ignore pattern rules

Patterns in `ignore_patterns` control which files are excluded from review and documentation checks:

| Pattern | Match rule |
|---|---|
| `*.generated.go` | No slash - matches by basename |
| `vendor/**` | Ends with `/**` - matches entire directory subtree |
| `some/path/file.go` | Contains a slash - full path glob matching |

## Environment variables

Bitbucket Cloud PR creation and CI monitoring use environment variables instead of a provider CLI:

- `NO_MISTAKES_BITBUCKET_EMAIL`
- `NO_MISTAKES_BITBUCKET_API_TOKEN`
- `NO_MISTAKES_BITBUCKET_API_BASE_URL` - optional API base URL override

See [environment variables](/no-mistakes/reference/environment/) for the full listing.
