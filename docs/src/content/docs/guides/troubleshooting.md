---
title: Troubleshooting
description: Common problems and how to debug them.
---

Most problems fall into one of three buckets: daemon not running, agent not
found, or push not triggering the pipeline. This page walks each one.

First stop for anything: `no-mistakes doctor`.

## Debug in this order

```mermaid
flowchart TD
  problem["Something is wrong"] --> doctor["Run no-mistakes doctor"]
  doctor --> daemon{"Daemon issue?"}
  daemon -- "yes" --> daemonpath["Check daemon status and daemon.log"]
  daemon -- "no" --> triggered{"Did the push trigger a run?"}
  triggered -- "no" --> gate["Check remote, hook, and socket"]
  triggered -- "yes" --> provider["Check agent or provider setup"]
```

That order matches the actual boundaries in the system:

- local environment and binaries
- daemon and gate wiring
- provider-specific PR or CI integration

## Daemon won't start

Symptoms: `no-mistakes daemon status` shows stopped, or `no-mistakes` exits with "daemon not running."

### Start it manually

```sh
no-mistakes daemon start
```

This installs or refreshes the managed service (launchd, systemd user service, or Task Scheduler), then starts it. If service install or startup fails, it falls back to a detached daemon.

### Check logs

```sh
tail -f ~/.no-mistakes/logs/daemon.log
```

### Check for stale artifacts

Stale PID files or sockets from a crashed daemon can block startup:

```sh
ls -la ~/.no-mistakes/daemon.pid ~/.no-mistakes/socket
```

If the PID file points at a process that's no longer running, remove both and run `no-mistakes daemon start` again.

### Managed service logs

- **macOS (launchd):** `launchctl list | grep no-mistakes` and check `~/Library/LaunchAgents/com.kunchenguid.no-mistakes.daemon.*.plist`
- **Linux (systemd):** `systemctl --user status no-mistakes-daemon-*` and `journalctl --user -u no-mistakes-daemon-* -f`
- **Windows (Task Scheduler):** `schtasks /query /tn "no-mistakes-daemon-*"`

### `NM_HOME` collisions

If you have multiple installs with different `NM_HOME` roots, each gets its own scoped service name (with a short suffix derived from the path). Make sure you're looking at the right one - `no-mistakes daemon status` reports which.

## `no-mistakes update` aborts

Symptom: `update` says "aborted: daemon running from different executable path."

The update requires the running daemon to already be using the same binary that's running the update. This is a safety check so you don't replace a binary that's been copied somewhere else.

Fix:

```sh
no-mistakes daemon stop
no-mistakes update
```

If the daemon executable path can't be determined at all (stale PID, permissions), the update also aborts. Same fix.

## Agent binary not detected

Symptom: `doctor` shows `–` for your agent, or the pipeline errors with "agent binary not found."

### Check PATH

The daemon uses the same binary-discovery order described in [Choosing an Agent](/no-mistakes/guides/agents/). When it's running through a managed service, it reloads `PATH` from your login shell on macOS and Linux, but it can't read Homebrew shellenv or nvm-style setups that only run in interactive shells.

Fastest fix: set an explicit override in `~/.no-mistakes/config.yaml`:

```yaml
agent_path_override:
  claude: /Users/you/.local/bin/claude
```

### Restart the daemon after installing a new agent

```sh
no-mistakes daemon stop
no-mistakes daemon start
```

## `git push no-mistakes` doesn't start a pipeline

Symptom: push succeeds but `no-mistakes` shows no active run.

### Check the remote

```sh
git remote -v | grep no-mistakes
```

If it's missing, run `no-mistakes init` again.

### Check the hook

The gate's bare repo has a `post-receive` hook that notifies the daemon. Look at the gate path:

```sh
no-mistakes status
# gate path is shown in the output

ls -la <gate-path>/hooks/post-receive
```

The hook should be executable. If it's missing or non-executable, `no-mistakes init` will reinstall it.

### Check the daemon socket

The hook talks to the daemon over `~/.no-mistakes/socket`. If the daemon isn't running, the push still succeeds (the hook never blocks), but no pipeline starts. Start the daemon and push again.

## PR step is skipped

Symptom: pipeline completes but the PR step shows `skipped`.

Check the [Provider Integration](/no-mistakes/guides/provider-integration/) requirements. Most common causes:

- `gh` or `glab` not installed
- `gh auth status` shows not authenticated
- Bitbucket env vars not set in the daemon's environment
- Upstream is on a host that isn't supported (GitHub, GitLab, or `bitbucket.org`)
- You pushed the default branch (PR step always skips on the default branch)

## CI step stuck or timed out

Symptom: CI step runs for 4 hours and pauses for approval.

`ci_timeout` defaults to `4h`. Raise it in `~/.no-mistakes/config.yaml`:

```yaml
ci_timeout: "8h"
```

If CI is genuinely hanging on the provider side, the step times out and pauses with findings for the unresolved state. You can approve (accept the risk), fix (run another auto-fix cycle), skip, or abort from the TUI.

## Worktree won't clean up

Symptom: `~/.no-mistakes/worktrees/<repoID>/<runID>/` sticks around after a run ends.

The daemon removes worktrees at run completion, and also on daemon startup (crash recovery). If one is still there:

```sh
# From inside the repo the worktree belongs to:
git worktree list
git worktree remove --force <path>
```

Or let the daemon clean it on next startup:

```sh
no-mistakes daemon stop
no-mistakes daemon start
```

## Reset everything

When state is genuinely wedged:

```sh
no-mistakes daemon stop
rm -rf ~/.no-mistakes/worktrees ~/.no-mistakes/socket ~/.no-mistakes/daemon.pid
no-mistakes daemon start
```

This keeps your gate repos, database, and config but clears transient state. For a full wipe, see the [Uninstall section](/no-mistakes/start-here/installation/#uninstall).

## Still stuck

- Check `~/.no-mistakes/logs/daemon.log` at `log_level: debug`
- File an issue: <https://github.com/kunchenguid/no-mistakes/issues>
- Discord: <https://discord.gg/Wsy2NpnZDu>
