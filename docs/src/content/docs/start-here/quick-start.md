---
title: Quick Start
description: Initialize no-mistakes and run your first gated push.
---

This walks you through your first gated push. For install options other than the macOS/Linux one-liner, see [Installation](/no-mistakes/start-here/installation/).

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

The installer drops the binary in `~/.no-mistakes/bin`, links it into `~/.local/bin` or `/usr/local/bin`, and restarts the background daemon. If the restart fails, the install command fails.

Official release binaries installed this way may already have telemetry enabled if a telemetry website ID was embedded at build time.

## 2. Check prerequisites

```sh
no-mistakes doctor
```

You need:

- `git`
- One supported agent binary (`claude`, `codex`, `acli` for Rovo Dev, or `opencode`)
- For PRs and CI: `gh` (GitHub), `glab` (GitLab), or Bitbucket Cloud credentials

See [Provider Integration](/no-mistakes/guides/provider-integration/) for PR/CI setup.

## 3. Initialize a repo

Navigate to any git repo with an `origin` remote:

```sh
no-mistakes init
```

This creates a local bare repo at `~/.no-mistakes/repos/<id>.git`, installs a post-receive hook, best-effort isolates the gate's hooks path from shared local Git config writes when Git supports `config --worktree`, adds a `no-mistakes` git remote to your working repo, and ensures the daemon is running.

```
$ no-mistakes init
  ✓ Gate initialized

    repo  /Users/you/src/my-repo
    gate  no-mistakes → /Users/you/.no-mistakes/repos/abc123def456.git
  remote  git@github.com:you/my-repo.git

  Push through the gate with:
  git push no-mistakes <branch>
```

`origin` is unchanged. If you need to bypass the gate for a specific push, use
`git push origin <branch>`.

## 4. Push through the gate

Instead of `git push origin`, push to the `no-mistakes` remote:

```sh
git checkout -b feature/login-fix
# do work, commit...
git push no-mistakes
```

The push lands in the local bare repo, the hook notifies the daemon, and the daemon starts the pipeline in a disposable worktree.

## 5. Watch the pipeline

```sh
no-mistakes
```

If the current branch has an active run, this attaches directly. If not, the setup wizard can walk you through creating a branch, committing, and pushing through the gate, then attach if the daemon registers the new run. By default that path is interactive in a TTY. With `no-mistakes -y`, the wizard accepts defaults automatically, stays visible and auto-advances in a TTY, and falls back to the headless path without a TTY.

The TUI shows each step's progress, streams agent output, and pauses for your approval when findings need attention. See [Using the TUI](/no-mistakes/guides/tui/) for keybindings and layout.

## What happens next

The pipeline runs these steps in order:

1. **Rebase** - onto the latest upstream
2. **Review** - AI code review of your diff
3. **Test** - your tests (configured command or agent-detected)
4. **Document** - checks for required doc updates
5. **Lint** - your linters (configured command or agent-detected)
6. **Push** - to the real upstream remote
7. **PR** - create or update the pull request
8. **CI** - poll CI, watch PR mergeability, auto-fix failures

Steps that find issues pause for your approval. See the [Pipeline concept page](/no-mistakes/concepts/pipeline/) for the overview and [Pipeline Steps](/no-mistakes/reference/pipeline-steps/) for each step's exact behavior.
