---
title: CLI Commands
description: Complete reference for all no-mistakes commands and flags.
---

## no-mistakes

Attach to the active pipeline run for the current branch when one exists. If none exists, bare `no-mistakes` can start the setup wizard to create a branch, commit changes, push through the gate, wait for the daemon to register the new run, and then attach. If the push succeeds but no run is registered, that wizard path now exits with an explicit error instead of silently falling through. By default this wizard path is interactive and only runs in a TTY session. In non-interactive contexts, bare `no-mistakes` falls back to showing the last 5 runs inline unless you pass `-y` or `--yes` to run the wizard and accept defaults automatically. When a TTY is available, `-y` keeps the wizard visible, shows a brief `waiting for run…` state after push, and auto-advances the default path; without a TTY it falls back to the headless path.

```sh
no-mistakes
no-mistakes --skip test,lint
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `-y`, `--yes` | `bool` | `false` | Run setup wizard and accept defaults automatically |
| `--skip` | `string` | (none) | Comma-separated pipeline steps to skip for a new run |

Unlike `no-mistakes attach`, bare `no-mistakes` only auto-attaches to an active run on the current branch.
`--skip` only applies when bare `no-mistakes` starts a new pipeline run through the wizard; it does not skip a step on an already-active run.
Valid step names are `intent`, `rebase`, `review`, `test`, `document`, `lint`, `push`, `pr`, and `ci`.

## no-mistakes init

Initialize or refresh the gate for the current repository.

```sh
no-mistakes init
```

Creates or refreshes a local bare repo, installs the post-receive hook, best-effort isolates the gate repo's hook path from shared git config changes when Git supports `config --worktree`, adds or repairs the `no-mistakes` git remote, detects the default branch, records or updates the repo in SQLite, installs the `/no-mistakes` agent skill into `.claude/skills/no-mistakes/SKILL.md` and `.agents/skills/no-mistakes/SKILL.md`, and ensures the daemon is running, installing the managed service when available and falling back to a detached daemon otherwise.
The gate advertises Git push-option support, so you can skip steps for one push with `git push -o no-mistakes.skip=test,lint no-mistakes <branch>`.

Re-running `init` on an already-initialized repo succeeds and reports `Gate already initialized (refreshed)`.
It refreshes managed gate wiring, origin/default-branch metadata, hook-path isolation, and the installed agent skill.
Fresh init rolls back gate setup when a required gate or daemon step fails; refresh does not eject a pre-existing gate if daemon startup fails.
Skill installation is best-effort: if the skill write fails, init reports it and leaves the working gate in place.

## no-mistakes axi

Agent eXperience Interface for non-interactive agents.
It prints TOON to stdout, prints progress to stderr, and uses structured stdout errors with exit code `1` for operational failures and `2` for bad usage.

```sh
no-mistakes axi
```

With no subcommand, shows the executable path, description, repo, daemon state, the active run when present, recent runs, and next-step help.

## no-mistakes axi run

Start or reattach to validation for the current branch, blocking until the first approval gate, CI-ready decision point, or final outcome.

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi run --intent "the user's goal" --skip test,lint
no-mistakes axi run --intent "the user's goal" --yes
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--intent` | `string` | (none) | What the user set out to accomplish; required to start a new run |
| `-y`, `--yes` | `bool` | `false` | Auto-resolve every gate until a decision point or outcome |
| `--skip` | `string` | (none) | Comma-separated pipeline steps to skip |

`--intent` is not a description of the diff.
It is the user's goal or request, and no-mistakes uses it verbatim instead of transcript inference.
Err on the side of completeness: include the goal, important decisions and tradeoffs, constraints or approaches ruled in or out, and explicit requests that might otherwise look surprising in the diff.
When starting a new run, `axi run` refuses the default branch and uncommitted working trees with actionable errors instead of auto-branching or auto-committing.
Reattaching to an in-flight run does not require `--intent`.
With `--yes`, `axi run` treats both `action: auto-fix` and `action: ask-user` findings as standing consent to fix them by selecting every finding, then accepts the resulting fix review.
Gates with no findings or only `action: no-op` findings are approved as-is, and each step is fixed at most once so unresolved findings do not loop forever.
Without `--yes`, an agent driving `axi run` should stop when a gate contains `action: ask-user` findings and relay each finding's ID, file, and full description to the user before responding.
When the CI step is still monitoring an open PR and checks are green, `axi run` exits successfully with `outcome: checks-passed` instead of waiting for a human merge.
Treat that as the agent stopping point: ask the user to review and merge the PR from the `help` line.

## no-mistakes axi respond

Answer the current approval gate and continue until the next gate, CI-ready decision point, or final outcome.

```sh
no-mistakes axi respond --action approve
no-mistakes axi respond --action fix --findings F1,F2 --instructions "optional guidance"
no-mistakes axi respond --action fix --add-finding '{"description":"...","action":"auto-fix"}'
no-mistakes axi respond --action skip
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--action` | `string` | (none) | `approve`, `fix`, or `skip`; required |
| `--step` | `string` | awaiting step | Step to respond to |
| `--findings` | `string` | (none) | Comma-separated finding IDs for `--action fix` |
| `--instructions` | `string` | (none) | Guidance applied to selected findings |
| `--add-finding` | `string` | (none) | JSON finding object to add and fix |
| `-y`, `--yes` | `bool` | `false` | Auto-resolve every subsequent gate until a decision point or outcome |

After the explicit response, `--yes` uses the same auto-resolution behavior as `axi run --yes`: fix `auto-fix` and `ask-user` findings once, approve the fix review, approve gates that only contain non-actionable `no-op` findings, and stop at `outcome: checks-passed` when CI is green but the PR still needs a human merge.

## no-mistakes axi status

Show the active run, or the most recent run when none is active.

```sh
no-mistakes axi status
no-mistakes axi status --run <id>
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--run` | `string` | active or most recent | Inspect a specific run ID |

## no-mistakes axi logs

Show the log output of one pipeline step.

```sh
no-mistakes axi logs --step review
no-mistakes axi logs --step review --full
no-mistakes axi logs --step review --run <id>
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--step` | `string` | (none) | Step name; required |
| `--run` | `string` | active or most recent | Run ID to inspect |
| `--full` | `bool` | `false` | Show the entire log instead of the tail |

Without `--full`, long logs show the last 40 lines and a help hint for the full log.

## no-mistakes axi abort

Cancel the active run for the current branch.

```sh
no-mistakes axi abort
```

If there is no active run, this succeeds as a no-op.

## no-mistakes eject

Remove the gate from the current repository.

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo directory, cleans up worktrees, and deletes the database record (cascades to runs and steps).
It does not remove repo-local agent skill files created by `init`.

## no-mistakes attach

Attach to the active pipeline run.

```sh
no-mistakes attach [--run <id>]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--run` | `string` | (none) | Attach to a specific run ID instead of the active run |

Opens the TUI for the active run anywhere in the current repo. If `--run` is specified, attaches to that specific run regardless of branch. Unlike bare `no-mistakes`, this does not stay branch-scoped before falling back.

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

## no-mistakes stats

Show historical usage stats across all repos.

```sh
no-mistakes stats
```

Displays total changes, rescued changes, rescue rate, reported and fixed mistakes, fixes by pipeline step, and the top repos by rescue activity.

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
- Native agent binaries: `claude`, `codex`, `acli`, `opencode`, `pi`

Uses indicators: `✓` (available), `–` (not found, optional), `✗` (problem detected).

`doctor` does not validate `acpx` or ACP targets. For `agent: acp:<target>`, verify `acpx_path` yourself.

`doctor` currently checks `gh` availability only. For GitLab PR and CI steps, install and authenticate `glab`. For Bitbucket Cloud PR and CI steps, set `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.

## no-mistakes update

Update the installed binary and reset the daemon.

```sh
no-mistakes update
no-mistakes update --beta
no-mistakes update -y
```

Downloads the latest release, verifies the SHA-256 checksum, atomically replaces the running binary, and resets the daemon when it is running or stale daemon artifacts exist so the new executable is picked up, preferring the managed service path and falling back to a detached daemon if service startup is unavailable or fails.
By default this installs the latest stable release.
Pass `--beta` to include prereleases and install the latest beta when one is newer than the current stable release.
If pending or running pipeline runs exist, update warns that restarting the daemon can cause those runs to fail, prints each active run's ID, status, branch, and short head SHA, and prompts before continuing.
If the daemon is running from a different executable path, update prompts before replacing it.
Pass `-y` or `--yes` to continue through update safety prompts while still printing warnings.
If the daemon executable path cannot be determined, the update aborts before replacement.
If the daemon does not come back cleanly after a successful replacement, the command reports that failure.
On macOS, removes the quarantine extended attribute.

Because `update` installs the latest official release binary, the replacement binary includes the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

Background update checks run automatically on each CLI invocation (except `update` itself). If a newer version is available, a notification is printed to stderr. Suppressed for dev builds or when `NO_MISTAKES_NO_UPDATE_CHECK=1` is set.

## no-mistakes daemon start

Start the daemon, installing or refreshing the managed service when possible.

```sh
no-mistakes daemon start
```

Prefers the managed service path and falls back to a detached daemon if service install or startup is unavailable or fails. If the daemon is already running, the command refreshes a stale macOS `launchd` or Linux `systemd` service definition and restarts through the managed service; if the definition is unchanged, it reports that the daemon is already running.

## no-mistakes daemon stop

Stop the running daemon process.

```sh
no-mistakes daemon stop
```

This does not remove the managed service. A later `no-mistakes`, `no-mistakes daemon start`, `init`, `attach`, `rerun`, or `update` can start the daemon again through the same service manager when available, or as a detached daemon otherwise.

## no-mistakes daemon restart

Restart the daemon.

```sh
no-mistakes daemon restart
```

Stops the current daemon and starts it again. This works whether the daemon is currently running or not.

## no-mistakes daemon status

Check whether the daemon is running.

```sh
no-mistakes daemon status
```

Shows the PID if the daemon is running.
