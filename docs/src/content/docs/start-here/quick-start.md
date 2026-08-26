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

Official release binaries installed this way include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## 2. Check prerequisites

```sh
no-mistakes doctor
```

You need:

- `git`
- One supported agent runner (`claude`, `codex`, `grok`, `acli` for Rovo Dev, `opencode`, `pi`, `copilot`, or `agy` for Antigravity), or a configured Cursor/ACP runner such as `agent: cursor`; see [Global Config](/no-mistakes/reference/global-config/) for ACP requirements
- For PRs and CI: `gh` (GitHub), `glab` (GitLab), `forgejo-axi` (Forgejo), Bitbucket Cloud credentials, `az` with the `azure-devops` extension (Azure DevOps), or `tea` (Gitea)

`no-mistakes doctor` reports whether the configured global runner can start a validation gate.
Every validation gate requires a runnable pipeline agent and otherwise fails before its first pipeline step.

See [Provider Integration](/no-mistakes/guides/provider-integration/) for PR/CI setup.

## 3. Initialize a repo

Navigate to any git repo with an `origin` remote:

```sh
no-mistakes init
```

This creates or refreshes a local bare repo at `~/.no-mistakes/repos/<id>.git`, installs managed pre- and post-receive hooks, best-effort isolates the gate's hooks path from shared local Git config writes when Git supports `config --worktree`, adds or repairs a `no-mistakes` git remote in your working repo, installs the `/no-mistakes` agent skill, and ensures the daemon is running.

For GitHub fork contributions, keep `origin` pointed at the parent repository and pass your fork as the push target:

```sh
no-mistakes init --fork-url git@github.com:you/my-repo.git
```

The gate will push validated branches to the fork while opening PRs against the parent.

```
$ no-mistakes init
  ✓ Gate initialized

    repo  /Users/you/src/my-repo
    gate  no-mistakes → /Users/you/.no-mistakes/repos/abc123def456.git
  remote  git@github.com:you/my-repo.git
   skill  /no-mistakes installed for agents at user level

  Push through the gate with:
  git push no-mistakes <branch>
```

`origin` is unchanged.
Without fork routing, you can bypass the gate for a specific push with `git push origin <branch>`.
With `--fork-url`, bypassing the gate means pushing to your fork URL yourself.

You can safely re-run `no-mistakes init` later to refresh gate wiring or update the installed agent skill after an upgrade.
If you rename or move the repo directory, re-run `no-mistakes init` from the new path to reattach the existing gate and keep its run history.
Copied repos get their own fresh gate while the original path still exists.

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

## Or let your agent run the gate

If you are already working inside a coding agent, you don't have to switch to the terminal.

### Set up the agent skill

`no-mistakes init` installs a version-matched global skill rather than adding a copy to each repository:

| Consumer | Installed location | Invoke |
| --- | --- | --- |
| Claude Code | `~/.claude/skills/no-mistakes/SKILL.md` | `/no-mistakes` |
| Pi | `~/.agents/skills/no-mistakes/SKILL.md` | `/skill:no-mistakes` or a plain-language request to validate with no-mistakes |
| Codex, OpenCode, and Rovo Dev | `~/.agents/skills/no-mistakes/SKILL.md` | Use the harness's normal skill invocation or ask it to validate with no-mistakes |

The installer follows an existing symlink between the Claude and vendor-neutral skill locations, so one physical copy can serve both. Grok Build is available as a pipeline runner but does not consume this driving skill.

Verify the installation:

```sh
no-mistakes --version
no-mistakes doctor
test -f ~/.agents/skills/no-mistakes/SKILL.md && echo "agent skill installed"
```

After upgrading or replacing the binary, run `no-mistakes init` again in an initialized repository to refresh the skill from that exact executable. Pi discovers global skills at startup; use `/reload` to refresh an active Pi session.

### Run it from the agent

In Claude Code, ask the agent to implement a task and gate it:

```
/no-mistakes add a --json flag to the status command
```

The equivalent explicit Pi invocation is:

```
/skill:no-mistakes add a --json flag to the status command
```

If the work is already committed on a feature branch, invoke the skill without a task to validate it. In task-first mode, the agent inspects scope, preserves unrelated work, commits only the task changes on a feature branch, and passes your task text as `--intent`. In validate-only mode, it validates the existing committed work.

Either way, the driving agent applies low-risk fixes itself and stops to relay findings that need your judgment. It drives the same gate as the TUI through `no-mistakes axi`, a non-interactive command surface that uses flags only, prints TOON on stdout, and exposes the same approval gates through `no-mistakes axi respond`.

See [Driving no-mistakes as an agent](/no-mistakes/guides/agents/#driving-no-mistakes-as-an-agent) for the full agent workflow.

## What happens next

The pipeline runs these steps in order:

1. **Intent** - use agent-supplied intent when present, otherwise infer author intent from recent local agent transcripts
2. **Rebase** - onto the latest upstream and pushed-branch target
3. **Review** - AI code review of your diff
4. **Test** - run targeted local checks and collect evidence for this change; this is not the repository's full CI suite
5. **Document** - update docs and report unresolved gaps
6. **Lint** - run configured lint/static analysis or agent-detected housekeeping
7. **Push** - deliver the validated commit to the configured push target
8. **PR** - create or update the pull request
9. **CI** - monitor the remote checks and workflow runs registered for the exact PR head, then repair actionable failures through a new Review → Test → Document → Lint cycle

Steps that find issues pause for your approval. A completed local **Test** step means focused change evidence passed; it does not mean remote CI is green. See [Local Test versus remote CI](/no-mistakes/concepts/pipeline/#local-test-versus-remote-ci) for that distinction and [Pipeline Steps](/no-mistakes/reference/pipeline-steps/) for each step's exact behavior.
