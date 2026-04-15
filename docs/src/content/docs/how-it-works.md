---
title: How It Works
description: Architecture and data flow of no-mistakes.
---

## The gate model

`no-mistakes` intercepts pushes by placing a local bare git repo between your working repo and the real upstream remote.

```
┌──────────────┐     git push no-mistakes <branch>     ┌─────────────────────┐
│ Your repo    │ ────────────────────────────────────► │ Local gate repo      │
│ origin       │                                       │ ~/.no-mistakes/...   │
│ no-mistakes  │ ◄─────────── added by init ────────── │ hooks/post-receive   │
└──────┬───────┘                                       └──────────┬──────────┘
       │                                                          │
       │                                    notifies daemon        │
       │                                                          ▼
       │                                               ┌─────────────────────┐
       │                                               │ Daemon              │
       │                                               │ SQLite + Unix socket│
       │                                               └──────────┬──────────┘
       │                                                          │
       │                                       creates detached worktree
       │                                                          ▼
       │                                               ┌─────────────────────┐
       │                                               │ Pipeline            │
        │                                               │ rebase → review     │
        │                                               │ test → document     │
        │                                               │ lint → push → pr    │
        │                                               │ → babysit           │
       │                                               └──────────┬──────────┘
       │                                                          │
       └────────────────────────────────────────────────────────► │ upstream
                                                                   └──────────
```

**Key design decisions:**

- **Named remote** - `origin` is never hijacked. You push to `no-mistakes` on purpose, so regular `git push` still works normally.
- **Disposable worktrees** - each run happens in its own detached worktree under `~/.no-mistakes/worktrees/`. The daemon can safely modify files, run tests, and commit fixes without touching your working directory.
- **Fixed pipeline** - the step order is opinionated and not configurable: `rebase → review → test → document → lint → push → pr → babysit`. What you _can_ configure is the commands each step runs and how many auto-fix attempts are allowed.

## Component overview

### Post-receive hook

When `git push no-mistakes <branch>` lands, the bare repo's `post-receive` hook fires. It calls `no-mistakes daemon notify-push` with the gate path, ref name, and old/new SHAs. The hook never blocks the push - it runs the notification in the background and always exits 0.

### Daemon

A long-running background process that manages pipeline runs. It:

- Listens on a Unix socket at `~/.no-mistakes/socket`
- Writes its PID to `~/.no-mistakes/daemon.pid`
- Serializes concurrent pushes to the same branch (new push cancels the in-progress run)
- Creates and cleans up worktrees
- Persists state to SQLite
- Streams events to connected TUI clients via IPC

The daemon auto-starts when needed (`init`, `attach`, `rerun`), and `update` resets it after replacing the binary when the daemon is running or stale daemon artifacts exist. You can also manage it explicitly with `no-mistakes daemon start|stop|status`.

On startup, the daemon recovers from crashes by marking any stuck runs as failed and cleaning up orphaned worktrees.

### Pipeline executor

The executor runs each step sequentially and manages the approval/fix loop. It can also end early after `rebase` if the branch has no diff against the default branch, marking the remaining steps as skipped.

1. Execute the step
2. If the step finds `action: auto-fix` findings and auto-fix is enabled, loop back with the agent to fix them (up to the configured limit)
3. If blocking findings remain, or any finding has `action: ask-user`, pause and wait for user action
4. `action: no-op` findings are informational only; the user can approve, fix selected findings, skip, or abort when the step pauses

### IPC

Communication between the CLI and daemon uses JSON-RPC 2.0 over the Unix socket. The `subscribe` method streams real-time events (step progress, log chunks, findings) to the TUI.

### Database

SQLite at `~/.no-mistakes/state.sqlite` tracks repos, runs, step results, and step rounds. Step rounds record each execution attempt (initial, auto-fix) with its own findings and duration. Legacy `user_fix` rounds are still read as `auto-fix` for backward compatibility.

## Local state

Everything lives under `~/.no-mistakes/` by default. Set `NM_HOME` to relocate it.

| Path | Contents |
|---|---|
| `state.sqlite` | SQLite database |
| `socket` | Unix domain socket for IPC |
| `daemon.pid` | Daemon process ID |
| `config.yaml` | Global configuration |
| `update-check.json` | Cached update check result |
| `repos/<id>.git` | Bare gate repos |
| `worktrees/<repoID>/<runID>/` | Disposable worktrees (cleaned up after each run) |
| `logs/<runID>/<step>.log` | Per-step log files |
| `logs/daemon.log` | Daemon log |

The repo ID is the first 6 bytes (12 hex chars) of `sha256(absolute_working_path)`.
