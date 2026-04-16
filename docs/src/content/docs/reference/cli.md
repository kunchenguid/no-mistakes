---
title: CLI Commands
description: Complete reference for all no-mistakes commands and flags.
---

## no-mistakes

Attach to the active pipeline run for the current repo. If no active run exists, shows the last 5 runs inline.

```sh
no-mistakes
```

Equivalent to `no-mistakes attach` when a run is active.

## no-mistakes init

Initialize the gate for the current repository.

```sh
no-mistakes init
```

Creates a local bare repo, installs the post-receive hook, adds the `no-mistakes` git remote, detects the default branch, records the repo in SQLite, and ensures the daemon is running, installing the managed service when available and falling back to a detached daemon otherwise.

Rolls back all changes if any step fails.

## no-mistakes eject

Remove the gate from the current repository.

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo directory, cleans up worktrees, and deletes the database record (cascades to runs and steps).

## no-mistakes attach

Attach to the active pipeline run.

```sh
no-mistakes attach [--run <id>]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--run` | `string` | (none) | Attach to a specific run ID instead of the active run |

Opens the TUI for the active run on the current branch. If `--run` is specified, attaches to that specific run regardless of branch.

## no-mistakes rerun

Rerun the pipeline for the current branch.

```sh
no-mistakes rerun
```

Starts a new pipeline run using the last-known head SHA on the current branch. Useful for retrying after a fix or configuration change.

## no-mistakes status

Show repo, daemon, and active run status.

```sh
no-mistakes status
```

Displays:
- Repo path and upstream URL
- Gate path
- Daemon status (running/stopped, PID)
- Active run details: ID, branch, status, head SHA, start time

## no-mistakes runs

List recorded pipeline runs for the current repo.

```sh
no-mistakes runs [--limit <n>]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--limit` | `int` | `10` | Maximum number of runs to display |

Shows runs newest-first with branch, status (styled), short SHA, timestamp, and PR URL if set.

## no-mistakes doctor

Check system health and dependencies.

```sh
no-mistakes doctor
```

Checks:
- `git` binary
- `gh` CLI (optional, needed for GitHub PR and CI steps)
- Data directory (`~/.no-mistakes/`)
- SQLite database
- Daemon status
- Agent binaries: `claude`, `codex`, `acli`, `opencode`

Uses indicators: `✓` (available), `–` (not found, optional), `✗` (problem detected).

For GitLab PR steps, install `glab`. For Bitbucket Cloud PR and CI steps, set `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.

## no-mistakes update

Update the installed binary and reset the daemon.

```sh
no-mistakes update
```

Downloads the latest release, verifies the SHA-256 checksum, atomically replaces the running binary, and resets the daemon when it is running or stale daemon artifacts exist so the new executable is picked up, preferring the managed service path and falling back to a detached daemon if service startup is unavailable or fails. If the daemon is running, update first requires it to already be using the same executable path as the binary running `no-mistakes update`; if that daemon executable path cannot be determined or it points to a different binary, the update aborts before replacement. If the daemon does not come back cleanly after a successful replacement, the command reports that failure. On macOS, removes the quarantine extended attribute.

Background update checks run automatically on each CLI invocation (except `update` itself). If a newer version is available, a notification is printed to stderr. Suppressed for dev builds or when `NO_MISTAKES_NO_UPDATE_CHECK=1` is set.

## no-mistakes daemon start

Start the daemon, installing or refreshing the managed service when possible.

```sh
no-mistakes daemon start
```

Prefers the managed service path and falls back to a detached daemon if service install or startup is unavailable or fails.

## no-mistakes daemon stop

Stop the running daemon process.

```sh
no-mistakes daemon stop
```

This does not remove the managed service. A later `no-mistakes daemon start`, `init`, `attach`, `rerun`, or `update` can start the daemon again through the same service manager when available, or as a detached daemon otherwise.

## no-mistakes daemon status

Check whether the daemon is running.

```sh
no-mistakes daemon status
```

Shows the PID if the daemon is running.
