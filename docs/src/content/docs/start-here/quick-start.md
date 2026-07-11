---
title: Quick start
description: Initialize no-mistakes and run your first gated push.
---

This walks you through your first gated push.
For install options other than the macOS and Linux one-liner, see [installation](/no-mistakes/start-here/installation/).

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

The installer drops the binary in `~/.no-mistakes/bin`, links it into `~/.local/bin` or `/usr/local/bin`, and restarts the background daemon.
If the restart fails, the install command fails.

Official release binaries installed this way include the default self-hosted telemetry host and website ID.
Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## 2. Check prerequisites

```sh
no-mistakes doctor
```

You need:

- `git`
- at least one routed runner CLI: `codex` (OpenAI provider) or `claude` (Anthropic provider)
- for PRs and CI: `gh` (GitHub), `glab` (GitLab), or Bitbucket Cloud credentials

Every model call is routed through profiles that pair a Codex candidate with a Claude backup, so one runner is enough and installing both adds provider failover.
`no-mistakes doctor` validates the routing contract, checks the runner binaries, and fails when a routed profile has no available candidate.

See [provider integration](/no-mistakes/guides/provider-integration/) for PR and CI setup.

## 3. Initialize a repo

Navigate to any git repo with an `origin` remote:

```sh
no-mistakes init
```

This creates or refreshes a local bare repo at `~/.no-mistakes/repos/<id>.git`, installs a post-receive hook, best-effort isolates the gate's hooks path from shared local Git config writes when Git supports `config --worktree`, adds or repairs a `no-mistakes` git remote in your working repo, installs the `/no-mistakes` agent skill, and ensures the daemon is running.

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

If the current branch has an active run, this attaches directly.
If not, the setup wizard can walk you through creating a branch, committing, and pushing through the gate, then attach if the daemon registers the new run.
By default that path is interactive in a TTY.
With `no-mistakes -y`, the wizard accepts defaults automatically, stays visible and auto-advances in a TTY, and falls back to the headless path without a TTY.

The TUI shows each step's progress, streams agent output, and pauses for your approval when findings need attention.
See [using the TUI](/no-mistakes/guides/tui/) for keybindings and layout.

## Or let your agent run the gate

If you are already working inside a coding agent like Claude Code, you do not have to switch to the terminal at all.
`no-mistakes init` installed the `/no-mistakes` skill at user level, available in every repo, so you can ask the agent to do a task and gate it:

```
/no-mistakes add a --json flag to the status command
```

Or, if the work is already committed on a feature branch, use bare `/no-mistakes` to validate it:

```
/no-mistakes
```

In task-first mode, the agent inspects scope, preserves unrelated work, commits only the task changes on a feature branch, and passes your task text as `--intent`.
In validate-only mode, it validates the existing committed work.
Either way, it lets the pipeline repair low-risk findings and stops to relay any finding that needs your judgment.
It drives the same gate as the TUI through `no-mistakes axi`, a non-interactive command surface that uses flags only, prints TOON on stdout, and exposes the same approval gates through `no-mistakes axi respond`.

See [driving no-mistakes as an agent](/no-mistakes/guides/agents/#driving-no-mistakes-as-an-agent) for the full agent workflow.

## What happens next

The pipeline runs these ten steps in order:

1. Intent - use agent-supplied intent when present, otherwise infer author intent from recent local agent transcripts.
2. Rebase - onto the latest upstream and pushed-branch target.
3. Review - AI code review of your diff; blocking findings enter the routed repair cascade.
4. Test - baseline tests plus evidence checks when intent is known.
5. Document - updates docs and reports unresolved gaps.
6. Lint - your linters, from the configured command or routed lint inspection.
7. Verify - a fresh, independent verification of the sealed candidate whenever anything changed after the strong review.
8. Push - transports the exact verified commit to the configured push target.
9. PR - create or update the pull request.
10. CI - poll CI, watch PR mergeability, and fix failures.

Steps that find issues pause for your approval.
Model selection for every step follows the [routing contract](/no-mistakes/reference/routing/).
See the [pipeline concept page](/no-mistakes/concepts/pipeline/) for the overview and [pipeline steps](/no-mistakes/reference/pipeline-steps/) for each step's exact behavior.
