---
title: Provider Integration
description: Set up GitHub, GitLab, or Bitbucket Cloud for PR creation and CI monitoring.
---

The PR and CI steps need to talk to your git host. Three hosts are supported:
GitHub, GitLab, and Bitbucket Cloud (`bitbucket.org`). Everything else
short-circuits the PR and CI steps with `skipped`.

Provider integration is optional for the local gate. You only need it for the
steps that happen after validation: opening or updating the PR, watching hosted
CI, and fixing remote-only failures.

Without any provider setup, `no-mistakes` still gives you the local gate:

- rebase
- review
- test
- document
- lint
- push through normal Git transport

What you do not get is PR automation and CI monitoring.

## What each step needs

| Step | GitHub | GitLab | Bitbucket Cloud |
|---|---|---|---|
| **PR** (create/update) | `gh` CLI, authenticated | `glab` CLI, authenticated | `NO_MISTAKES_BITBUCKET_EMAIL` + `NO_MISTAKES_BITBUCKET_API_TOKEN` |
| **CI** (polling, auto-fix) | `gh` CLI | `glab` CLI | same env vars |
| **Merge conflict auto-fix** | `gh` CLI | `glab` CLI | not supported |
| **Mergeability polling** | `gh` CLI | `glab` CLI | not supported |

## What changes when provider wiring is present

Once the host is wired up, `no-mistakes` can keep owning the branch after it
pushes to the configured target:

- create or update the PR automatically
- keep polling hosted CI until the PR is merged, closed, declined, or times out
- fetch failing job logs for the CI auto-fix loop
- on GitHub and GitLab, watch mergeability and fix merge conflicts when possible

## GitHub

Install the GitHub CLI and authenticate:

```sh
# macOS
brew install gh

# Linux
# see https://github.com/cli/cli/blob/trunk/docs/install_linux.md

gh auth login
```

Verify:

```sh
gh auth status
```

`no-mistakes doctor` also checks for `gh` availability.
For PR and workflow-run commands, no-mistakes passes the repository slug from the recorded upstream remote or PR URL to `gh`, so daemon-run commands do not depend on the daemon's current working directory.

**What you get:**

- PR creation and update on pushes
- CI check polling with exponential backoff (30s → 60s → 120s) until the PR is merged, closed, or times out
- Failed job log fetching (`gh run view --log-failed`) for the CI auto-fix step
- PR mergeability polling, and agent-driven merge-conflict resolution when the branch falls behind

### GitHub fork contributions

Fork routing is available for GitHub when you need to push branches to your fork but open PRs against the parent repository.
Keep `origin` pointed at the parent repository, then initialize with your fork URL:

```sh
git remote set-url origin git@github.com:parent-owner/repo.git
no-mistakes init --fork-url git@github.com:your-user/repo.git
```

With this setup, the push and CI auto-fix push steps update the fork, while the PR and CI steps stay scoped to the parent repository.
The GitHub PR step opens PRs with a fork-qualified head such as `your-user:feature-branch`.
Re-running `no-mistakes init` later preserves the stored fork URL unless you pass a new `--fork-url`.

Fork routing currently requires both `origin` and `--fork-url` to be GitHub remotes with owner/repo paths.
GitLab and Bitbucket fork MR/PR routing are not implemented yet; if a legacy or manually edited repo record has `fork_url` set for those providers, PR creation skips instead of opening an unsafe self PR.

## GitLab

Install the GitLab CLI and authenticate:

```sh
# macOS
brew install glab

# Linux
# see https://gitlab.com/gitlab-org/cli

glab auth login
```

**What you get:**

- PR (merge request) creation and update
- CI pipeline status polling until the merge request is merged, closed, or times out
- Failed job trace fetching (`glab ci trace`) for the CI auto-fix step
- Merge-conflict polling and auto-fix, same as GitHub

## Bitbucket Cloud

Bitbucket Cloud uses the REST API directly rather than a provider CLI. Set two environment variables (and optionally a third):

```sh
export NO_MISTAKES_BITBUCKET_EMAIL=you@example.com
export NO_MISTAKES_BITBUCKET_API_TOKEN=your-api-token

# Optional: override the API base URL
export NO_MISTAKES_BITBUCKET_API_BASE_URL=https://api.bitbucket.org/2.0
```

Get an API token from [Bitbucket account settings](https://bitbucket.org/account/settings/app-passwords/).

**What you get:**

- PR creation and update
- CI pipeline status polling until the PR is merged, declined, or times out
- Failed pipeline step log fetching for the CI auto-fix step

**What you don't get (yet):**

- PR mergeability polling
- Merge-conflict auto-fix

These are GitHub and GitLab only right now.

## Self-hosted GitHub/GitLab

Self-hosted GitHub Enterprise and self-hosted GitLab instances work through the same `gh` and `glab` CLIs. Authenticate the CLI against your instance (`gh auth login --hostname your-ghe.example.com`, `glab auth login --hostname gitlab.example.com`) and `no-mistakes` will route through the CLI as usual.

## Unsupported hosts

If your upstream isn't GitHub, GitLab, or Bitbucket Cloud:

- The **push** step still runs - `no-mistakes` pushes through git to the configured target like any other remote.
- The **PR** step marks itself as `skipped`.
- The **CI** step marks itself as `skipped`.

Everything before push (rebase, review, test, document, lint) still works regardless of host. If your host has a CLI that exposes CI status and PR state, open an issue - new providers are straightforward to add.

## Checking what's wired up

```sh
no-mistakes doctor
```

`doctor` currently checks `gh` availability. For GitLab, confirm `glab` is installed and authenticated. For Bitbucket Cloud, confirm the two env vars are set in the environment the daemon runs under.

:::note
When the daemon runs through a managed service (launchd, systemd, Task Scheduler), it reloads environment from your login shell on macOS and Linux so `gh` auth and `NO_MISTAKES_BITBUCKET_*` vars are picked up, and it augments `PATH` with common binary directories. If credentials or PATH-derived tools are missing, check `~/.no-mistakes/logs/daemon.log` for a login-shell environment resolution warning. On Windows it reuses the current process environment.
:::
