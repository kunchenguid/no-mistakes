---
title: Environment variables
description: All environment variables recognized by no-mistakes.
---

## `NM_HOME`

Override the data directory.

| | |
|---|---|
| Type | `string` |
| Default | `~/.no-mistakes` |

When set, everything else moves under this root:

- Global config: `$NM_HOME/config.yaml`
- Gate repos: `$NM_HOME/repos/<id>.git`
- Worktrees: `$NM_HOME/worktrees/<repoID>/<runID>/`
- Logs: `$NM_HOME/logs/`
- Database: `$NM_HOME/state.sqlite`
- Socket / PID: `$NM_HOME/socket` and `$NM_HOME/daemon.pid`
- Managed agent server PID records: `$NM_HOME/servers/`
- Read-command telemetry sampling state: `$NM_HOME/telemetry-gate.json`
- Managed service names get a short stable suffix derived from `$NM_HOME` so multiple installs don't collide.

## `NO_MISTAKES_BITBUCKET_EMAIL`

Bitbucket Cloud account email used for PR creation and CI monitoring.

| | |
|---|---|
| Type | `string` |
| Default | (none; Bitbucket PR/CI steps skip when unset) |

Used alongside `NO_MISTAKES_BITBUCKET_API_TOKEN`. See [provider integration](/no-mistakes/guides/provider-integration/#bitbucket-cloud).

## `NO_MISTAKES_BITBUCKET_API_TOKEN`

Bitbucket Cloud API token.

| | |
|---|---|
| Type | `string` |
| Default | (none) |

Get one from [Bitbucket account settings](https://bitbucket.org/account/settings/app-passwords/).

## `NO_MISTAKES_BITBUCKET_API_BASE_URL`

Override the Bitbucket Cloud API base URL.

| | |
|---|---|
| Type | `string` |
| Default | `https://api.bitbucket.org` |

Useful for mocking in tests or pointing at a proxy.
no-mistakes appends the `/2.0` API path to every request, so do not include `/2.0` in the override.

## `NO_MISTAKES_NO_UPDATE_CHECK`

Disable background update checks.

| | |
|---|---|
| Type | `1` to disable, anything else to leave enabled |
| Default | unset (checks enabled) |

Update checks run on every CLI invocation except `update` itself, hit GitHub releases, cache the result in `$NM_HOME/update-check.json`, and print a one-line notification to stderr when a newer version is available. Dev builds (non-semver versions) suppress the check automatically.

## `XDG_DATA_HOME`

Data directory used to discover OpenCode transcripts for intent extraction.

| | |
|---|---|
| Type | `string` |
| Default | `~/.local/share` |

When set, no-mistakes looks for OpenCode's intent transcript database at `$XDG_DATA_HOME/opencode/opencode.db`.
When unset, it falls back to `~/.local/share/opencode/opencode.db`.

## `NO_MISTAKES_UMAMI_HOST`

Override the telemetry collection host.

| | |
|---|---|
| Type | `URL` |
| Default | `https://a.kunchenguid.com` |

When set, telemetry sends events to this host's `/api/send` endpoint. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_HOST`. If no runtime value is found, it falls back to any host embedded at build time and then the default self-hosted Umami instance.

## `NO_MISTAKES_UMAMI_WEBSITE_ID`

Override or enable the telemetry website ID.

| | |
|---|---|
| Type | `string` |
| Default | embedded in Makefile and release builds; unset in unembedded dev builds |

When set, telemetry uses this website ID at runtime. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_WEBSITE_ID`. If no runtime value is found, it falls back to any website ID embedded at build time.

When telemetry is enabled, `no-mistakes` sends command, run, approval, fix, and wizard events.
It also sends completed step events with `awaiting_approval`, `fix_review`, or `failed` status.
Human pageviews use `/wizard` and `/tui`.
Non-polling AXI pageviews use `/axi/run`, `/axi/respond`, `/axi/abort`, `/axi/wizard`, and `/axi/canary`.
Each AXI pageview is paired with a command event so command status and duration remain available.

The high-frequency read-only commands `axi`, `axi status`, `axi logs`, `status`, and `runs` send no pageviews.
They send one command event on the first observation, immediately after a local state fingerprint changes, or after a 10-minute heartbeat while state remains unchanged.
The deduplication state persists across CLI processes in `$NM_HOME/telemetry-gate.json`.
Sampler read, lock, or write failures fail open and can cause an extra event rather than hiding a meaningful state change.
The local fingerprint is never included in the remote event.

Remote AXI context is limited to bounded flag-derived fields.
`/axi/run` records only whether `--yes`, `--intent`, or `--skip` was present, not the intent or skip values.
`/axi/respond` records the sanitized `approve`, `fix`, or `skip` action and whether `--yes` was present.
The sampled `axi status` command records only whether `--run` was present.
The sampled `axi logs` command records a registered step name or `invalid`, whether `--full` was present, and whether `--run` was present.
No raw run ID is sent.
The other listed AXI surfaces carry no flag-derived context.

`no-mistakes stats`, including `--agents` and `--run <id>`, sends only the ordinary command name, status, and duration.
Terminal run events add only `agent_invocations`, `resumed_invocations`, and `fallback_invocations` counts.
Detailed local invocation records are not sent remotely.
The terminal performance roll-up does not include run IDs, paths, providers, models, session keys, timings, token counts, prompts, model output, diffs, credentials, or raw errors.
See [`no-mistakes stats`](/no-mistakes/reference/cli/#no-mistakes-stats) for the local report.

## `NO_MISTAKES_TELEMETRY`

Disable telemetry collection.

| | |
|---|---|
| Type | `0`, `false`, or `off` to disable; anything else to leave enabled |
| Default | unset |

When set to a disabling value, telemetry stays off even if a runtime or embedded website ID is available.

## Environment the daemon sees

When the daemon runs through a managed service (launchd, systemd user service, Task Scheduler), the macOS and Linux service definitions include a default `PATH` with common user and system binary directories.
At startup on macOS and Linux, the daemon resolves the environment from your login shell once and caches it for the life of the daemon process.
It preserves your shell `PATH` order and appends any missing well-known directories such as `~/.local/bin`, `~/go/bin`, `~/.cargo/bin`, `~/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, and `/bin`.
If login-shell resolution fails or returns no entries, the daemon logs a warning and uses an augmented process-environment fallback that may omit version-manager directories such as nvm, fnm, or volta.
On Windows it reuses the current process environment.

If your env vars aren't set in your login shell's rc files (`.zprofile`, `.zshrc`, `.profile`, `.bash_profile`, `.bashrc`, PowerShell profile), the daemon won't see them. Put them somewhere a login shell will load.
Because the environment is cached at startup, the next run does not pick up changes on its own: run `no-mistakes daemon restart` after changing it.
