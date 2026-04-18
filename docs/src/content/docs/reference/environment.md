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

## `NO_MISTAKES_UMAMI_WEBSITE_ID`

Override or enable the telemetry website ID.

| | |
|---|---|
| Type | `string` |
| Default | unset |

When set, telemetry uses this website ID at runtime. If it is unset in a dev build, `no-mistakes` also checks a repo-local `.env` file for `NO_MISTAKES_UMAMI_WEBSITE_ID`. If no runtime value is found, it falls back to any website ID embedded at build time.

## Environment the daemon sees

When the daemon runs through a managed service (launchd, systemd user service, Task Scheduler), it reloads environment from your login shell on macOS and Linux before each run so `PATH` and `NO_MISTAKES_*` vars match what you'd see in an interactive shell. On Windows it reuses the current process environment.

If your env vars aren't set in your login shell's rc files (`.zprofile`, `.zshrc`, `.profile`, `.bash_profile`, `.bashrc`, PowerShell profile), the daemon won't see them. Put them somewhere a login shell will load.
