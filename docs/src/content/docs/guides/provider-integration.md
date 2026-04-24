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

What you do not get is PR automation and CI babysitting.

## What each step needs

| Step | GitHub | GitLab | Bitbucket Cloud |
|---|---|---|---|
| **PR** (create/update) | `gh` CLI, authenticated | `glab` CLI, authenticated | `NO_MISTAKES_BITBUCKET_EMAIL` + `NO_MISTAKES_BITBUCKET_API_TOKEN` |
| **CI** (polling, auto-fix) | `gh` CLI | `glab` CLI | same env vars |
| **Merge conflict auto-fix** | `gh` CLI | `glab` CLI | not supported |
| **Mergeability polling** | `gh` CLI | `glab` CLI | not supported |

## What changes when provider wiring is present

Once the host is wired up, `no-mistakes` can keep owning the branch after the
upstream push:

- create or update the PR automatically
- poll hosted CI without you refreshing a browser tab
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

**What you get:**

- PR creation and update on pushes
- CI check polling with exponential backoff (30s → 60s → 120s)
- Failed job log fetching (`gh run view --log-failed`) for the CI auto-fix step
- PR mergeability polling, and agent-driven merge-conflict resolution when the branch falls behind

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
- CI pipeline status polling
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
- CI pipeline status polling
- Failed pipeline step log fetching for the CI auto-fix step

**What you don't get (yet):**

- PR mergeability polling
- Merge-conflict auto-fix

These are GitHub and GitLab only right now.

## Self-hosted GitHub/GitLab

Self-hosted GitHub Enterprise and self-hosted GitLab instances work through the same `gh` and `glab` CLIs. Authenticate the CLI against your instance (`gh auth login --hostname your-ghe.example.com`, `glab auth login --hostname gitlab.example.com`) and `no-mistakes` will route through the CLI as usual.

## Unsupported hosts

If your upstream isn't GitHub, GitLab, or Bitbucket Cloud:

- The **push** step still runs - `no-mistakes` pushes through git like any other remote.
- The **PR** step marks itself as `skipped`.
- The **CI** step marks itself as `skipped`.

Everything before push (rebase, review, test, document, lint) still works regardless of host. If your host has a CLI that exposes CI status and PR state, open an issue - new providers are straightforward to add.

## Checking what's wired up

```sh
no-mistakes doctor
```

`doctor` currently checks `gh` availability. For GitLab, confirm `glab` is installed and authenticated. For Bitbucket Cloud, confirm the two env vars are set in the environment the daemon runs under.

:::note
When the daemon runs through a managed service (launchd, systemd, Task Scheduler), it reloads environment from your login shell on macOS and Linux so `gh` auth and `NO_MISTAKES_BITBUCKET_*` vars are picked up, and it augments `PATH` with common binary directories. On Windows it reuses the current process environment.
:::
