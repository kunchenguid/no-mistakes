---
title: Getting Started
description: Install no-mistakes and run your first gated push.
---

## Install

### macOS / Linux

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.ps1 | iex
```

### Go install

```sh
go install github.com/kunchenguid/no-mistakes/cmd/no-mistakes@latest
```

### From source

```sh
git clone git@github.com:kunchenguid/no-mistakes.git
cd no-mistakes
make build
make install
```

## Prerequisites

- **git** - required
- **One supported agent binary** - `claude`, `codex`, `acli` (Rovo Dev), or `opencode`
- **gh** (GitHub CLI) - optional, needed for PR creation and CI babysitting

Run `no-mistakes doctor` to check what's installed and ready.

## Initialize a repo

Navigate to any git repository with an `origin` remote and run:

```sh
no-mistakes init
```

This does the following:

1. Creates a local bare git repo at `~/.no-mistakes/repos/<id>.git`
2. Installs a `post-receive` hook in that bare repo
3. Adds a git remote named `no-mistakes` to your working repo
4. Starts the background daemon (if not already running)
5. Records the repo in the local SQLite database

```
$ no-mistakes init
initialized gate for /Users/you/src/my-repo
  remote: no-mistakes -> /Users/you/.no-mistakes/repos/abc123def456.git
  upstream: git@github.com:you/my-repo.git

Push through the gate with: git push no-mistakes <branch>
```

## Push through the gate

Instead of `git push origin`, push to the `no-mistakes` remote:

```sh
git push no-mistakes feature/login-fix
```

The push lands in the local bare repo. The hook fires, notifies the daemon, and the daemon starts the validation pipeline in a disposable worktree.

## Watch the pipeline

Run `no-mistakes` (or `no-mistakes attach`) to open the TUI and watch the pipeline run:

```sh
no-mistakes
```

The TUI shows each step's progress, streams agent output, and pauses for your approval when findings need attention. See the [TUI guide](/no-mistakes/guides/tui/) for keybindings and layout.

## What happens next

The pipeline runs these steps in order:

1. **Rebase** - rebase onto the latest upstream
2. **Review** - AI code review of your diff
3. **Test** - run tests (configured command or agent-detected)
4. **Lint** - run linters (configured command or agent-detected)
5. **Push** - push to the real upstream remote
6. **PR** - create or update a pull request
7. **Babysit** - poll CI and auto-fix failures

Steps that find issues pause for your approval. You can approve, fix, skip, or abort. See [Pipeline Steps](/no-mistakes/guides/pipeline-steps/) and [Auto-Fix](/no-mistakes/guides/auto-fix/) for details.

## Update

To update an existing install in place:

```sh
no-mistakes update
```

This downloads the latest release from GitHub, verifies the SHA-256 checksum, and atomically replaces the binary.

## Remove from a repo

```sh
no-mistakes eject
```

This removes the `no-mistakes` remote, deletes the bare repo, cleans up worktrees, and removes the database record.
