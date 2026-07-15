# no-mistakes — local setup notes

Machine-specific notes for this install, on top of the main [README](README.md).

## What's installed here

- Binary: `%LOCALAPPDATA%\no-mistakes\no-mistakes.exe` (added to user `PATH`)
- Daemon: runs persistently in the background, auto-started by `daemon restart`
- Skill: `/no-mistakes` installed at the **user level** (`~/.claude/skills/no-mistakes`), so it's available in any repo, not just this one
- Gate repo: `~/.no-mistakes/repos/<hash>.git` — the bare repo `git push no-mistakes` actually pushes to before the pipeline forwards it on

## Three ways to trigger the gate

| Method | When to use |
|---|---|
| `git push no-mistakes <branch>` | You already committed and just want to push through the gate |
| `no-mistakes` (TUI) | Uncommitted changes — wizard branches/commits/pushes for you. `no-mistakes -y` accepts defaults automatically |
| `/no-mistakes <task>` | Tell your coding agent to do a task and gate it; bare `/no-mistakes` gates existing committed work |

The pipeline is always: **review → test → docs → lint → push → PR → CI**, run in a disposable worktree so your working directory is untouched.

## Findings: what's automatic vs what needs you

- **Auto-fix findings** — safe, mechanical fixes are applied without asking
- **Ask-user findings** — anything that touches intent stops and asks you to **approve**, **fix**, or **skip**
- Nothing reaches the real remote / opens a PR until every check is green

## Useful commands

```sh
no-mistakes status          # status of the current repo's gate
no-mistakes runs            # list past pipeline runs
no-mistakes attach          # attach to the currently active run
no-mistakes rerun           # rerun the pipeline for the current branch
no-mistakes doctor          # check system health / dependencies
no-mistakes eject           # remove the gate from the current repo

no-mistakes daemon status   # check if the background daemon is running
no-mistakes daemon stop     # stop it (e.g. to pause auto-push/auto-fix behavior)
no-mistakes daemon restart  # restart it
no-mistakes update          # update the binary and reset the daemon
```

## Fork note

This repo (`landonbrice/no-mistakes-os`) is an unmodified fork of `kunchenguid/no-mistakes` used as a personal gate-init target. The installed binary itself comes from upstream's GitHub releases — this fork has none published, and its own `install.sh`/`install.ps1` still point at upstream.
