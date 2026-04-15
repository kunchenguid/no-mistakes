---
title: Daemon
description: Background process management and recovery.
---

The daemon is a long-running background process that manages pipeline runs. The installer sets it up as a managed background service, and `init`, `attach`, `rerun`, and `update` keep that service installed and running for you.

## Starting and stopping

```sh
# Explicit management
no-mistakes daemon start
no-mistakes daemon stop
no-mistakes daemon status

# Ensures the managed daemon service is installed and running
no-mistakes init
no-mistakes attach
no-mistakes rerun

# Resets the daemon after replacing the binary
no-mistakes update
```

`no-mistakes update` stops and starts the daemon service when it is running, or when stale daemon artifacts exist, so the new executable is used. If the daemon is already running, update only proceeds when the daemon is using the same executable path as the binary running the update command; if that path cannot be determined or points to a different binary, the update aborts before replacing anything.

The daemon writes its PID to `~/.no-mistakes/daemon.pid` and listens on a Unix socket at `~/.no-mistakes/socket`. On Windows, it uses a localhost TCP listener and a protected endpoint file at the same path.

## What it does

When a push arrives via the post-receive hook:

1. Creates a detached worktree at `~/.no-mistakes/worktrees/<repoID>/<runID>/`
2. Starts the pipeline executor in that worktree
3. Streams events to any connected TUI clients
4. Cleans up the worktree when the run finishes (success or failure)

## Concurrent push handling

If you push to the same branch while a run is already active, the daemon:

1. Cancels the in-progress run (reason: "cancelled: superseded by new push")
2. Waits for it to finish
3. Starts a new run with the latest push

Pushes to different branches run concurrently.

## Crash recovery

On startup, the daemon checks for runs that were left in `pending` or `running` status (which means the daemon crashed while they were active):

- Marks those runs as `failed` with the message "daemon crashed during execution"
- Removes any orphaned worktree directories via `git worktree remove --force`

## Logging

Daemon logs go to `~/.no-mistakes/logs/daemon.log`. Each pipeline step also writes to its own log at `~/.no-mistakes/logs/<runID>/<step>.log`.

Set the log level in global config:

```yaml
log_level: debug  # debug | info | warn | error
```

## Shutdown

`no-mistakes daemon stop` stops the current daemon process without removing the managed service:

1. Cancels all active runs
2. Waits up to 30 seconds for goroutines to finish
3. Removes the PID file and socket
