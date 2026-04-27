---
title: Environment Variables
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
- Managed service names get a short stable suffix derived from `$NM_HOME` so multiple installs don't collide.

## `NO_MISTAKES_BITBUCKET_EMAIL`

Bitbucket Cloud account email used for PR creation and CI monitoring.

| | |
|---|---|
| Type | `string` |
| Default | (none; Bitbucket PR/CI steps skip when unset) |

Used alongside `NO_MISTAKES_BITBUCKET_API_TOKEN`. See [Provider Integration](/no-mistakes/guides/provider-integration/#bitbucket-cloud).

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
| Default | `https://api.bitbucket.org/2.0` |

Useful for mocking in tests or pointing at a proxy.

## `NO_MISTAKES_NO_UPDATE_CHECK`

Disable background update checks.

| | |
|---|---|
| Type | `1` to disable, anything else to leave enabled |
| Default | unset (checks enabled) |

Update checks run on every CLI invocation except `update` itself, hit GitHub releases, cache the result in `$NM_HOME/update-check.json`, and print a one-line notification to stderr when a newer version is available. Dev builds (non-semver versions) suppress the check automatically.

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
| Default | embedded in release builds |

When set, telemetry uses this website ID at runtime. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_WEBSITE_ID`. If no runtime value is found, it falls back to any website ID embedded at build time.

When telemetry is enabled, `no-mistakes` sends command, run, approval, fix, and wizard events, completed step events with `awaiting_approval`, `fix_review`, or `failed` status, and pageviews for `/wizard` and `/tui` to Umami.

## `NO_MISTAKES_TELEMETRY`

Disable telemetry collection.

| | |
|---|---|
| Type | `0`, `false`, or `off` to disable; anything else to leave enabled |
| Default | unset |

When set to a disabling value, telemetry stays off even if a runtime or embedded website ID is available.

## Environment the daemon sees

When the daemon runs through a managed service (launchd, systemd user service, Task Scheduler), the macOS and Linux service definitions include a default `PATH` with common user and system binary directories. Before each run, the daemon reloads environment from your login shell on macOS and Linux, preserves your shell `PATH` order, and appends any missing well-known directories such as `~/.local/bin`, `~/go/bin`, `~/.cargo/bin`, `~/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, and `/bin`. On Windows it reuses the current process environment.

If your env vars aren't set in your login shell's rc files (`.zprofile`, `.zshrc`, `.profile`, `.bash_profile`, `.bashrc`, PowerShell profile), the daemon won't see them. Put them somewhere a login shell will load.
