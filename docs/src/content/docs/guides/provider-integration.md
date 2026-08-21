---
title: Provider Integration
description: Set up GitHub, GitLab, Forgejo, Bitbucket Cloud, or Azure DevOps for PR creation and CI monitoring.
---

The PR and CI steps need to talk to your git host. Five hosts are supported:
GitHub, GitLab, Forgejo, Bitbucket Cloud (`bitbucket.org`), and Azure DevOps
(`dev.azure.com` and legacy `*.visualstudio.com`). Everything else
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

| Step | GitHub | GitLab | Forgejo | Bitbucket Cloud | Azure DevOps |
| --- | --- | --- | --- | --- | --- |
| **PR** (create/update) | `gh` CLI, authenticated | `glab` CLI, authenticated | `forgejo-axi`, authenticated | `NO_MISTAKES_BITBUCKET_EMAIL` + `NO_MISTAKES_BITBUCKET_API_TOKEN` | `az` CLI + `azure-devops` extension, authenticated |
| **CI** (polling, auto-fix) | `gh` CLI | `glab` CLI | `forgejo-axi` | same env vars | `az` CLI |
| **Merge conflict auto-fix** | `gh` CLI | `glab` CLI | `forgejo-axi` | not supported | `az` CLI |
| **Mergeability polling** | `gh` CLI | `glab` CLI | `forgejo-axi` | not supported | `az` CLI |
| **Failed check log fetching** | `gh` CLI | `glab` CLI | `forgejo-axi` when runtime routes are available | supported | not yet |
| **[Cancelled-check rerun](/no-mistakes/reference/repo-config/#cirerun_transient)** | `gh` CLI | not supported | not supported | not supported | not supported |

## What changes when provider wiring is present

Once the host is wired up, `no-mistakes` can keep owning the branch after it
pushes to the configured target:

- create or update the PR automatically
- keep polling hosted CI until the PR is merged, closed, declined, or the configured `ci_timeout` idle window elapses
- fetch failing job logs for the CI auto-fix loop when the provider exposes them
- on GitHub, GitLab, Forgejo, and Azure DevOps, watch mergeability and fix merge conflicts when possible

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
- CI check polling with exponential backoff (30s → 60s → 120s) until the PR is merged, closed, or the configured `ci_timeout` idle window elapses
- Failed job log fetching (`gh run view --log-failed`) for the CI auto-fix step
- PR mergeability polling, and agent-driven resolution when the provider reports an actual merge conflict

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
GitLab, Forgejo, Bitbucket, and Azure DevOps fork MR/PR routing are not implemented yet; if a legacy or manually edited repo record has `fork_url` set for those providers, PR creation skips instead of opening an unsafe self PR.

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
- CI pipeline status polling until the merge request is merged, closed, or the configured `ci_timeout` idle window elapses
- Failed job trace fetching (`glab ci trace`) for the CI auto-fix step
- Merge-conflict polling and auto-fix, same as GitHub

## Forgejo

Install [`forgejo-axi`](https://github.com/escidmore/forgejo-axi) and make it available on `PATH`. It currently installs from source with Node.js 20 or newer; set [`forgejo_axi_path`](/no-mistakes/reference/global-config/#forgejo_axi_path) when the executable lives elsewhere.

Give the daemon a Forgejo token through either the generic `FORGEJO_TOKEN` variable or forgejo-axi's host-scoped token variable. Configure `FORGEJO_BASE_URL` for SSH origins and unrecognized self-hosted HTTPS hostnames. The [environment reference](/no-mistakes/reference/environment/#forgejo_base_url) owns the exact base-URL and host-key rules.

Verify without mutating a deployed Forgejo instance:

```sh
FORGEJO_BASE_URL=https://forgejo.example forgejo-axi status --json
```

`no-mistakes` delegates PR identity, lifecycle, commit-status and required-context evaluation, mergeability, and merged proof to forgejo-axi's stable JSON commands. Capabilities are runtime-probed from the server instead of guessed from a major version.

Failed-log retrieval uses forgejo-axi's stable `run view --log-failed --json` command only when the runtime probe advertises commit statuses, Actions runs, run jobs, and job logs. `no-mistakes` accepts only a failing status's canonical native Actions target, verifies the exact run and freshly polled PR head, validates every returned job's run identity, and returns non-empty failed-job logs in deterministic order under the aggregate 1 MiB ceiling. Live testing against Forgejo 16.0.1 proves identity-matched retrieval through those routes. On Forgejo 15.0.5, commit-status gating, mergeability checks, and merged-state proof remain available when job and log routes are unavailable.

Fork PR routing is not implemented for Forgejo; a configured `fork_url` makes the PR step skip rather than opening a self PR.

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
- CI pipeline status polling until the PR is merged, declined, or the configured `ci_timeout` idle window elapses
- Failed pipeline step log fetching for the CI auto-fix step

**What you don't get (yet):**

- PR mergeability polling
- Merge-conflict auto-fix

These are GitHub, GitLab, Forgejo, and Azure DevOps only right now.

## Azure DevOps

Azure DevOps uses the Azure CLI with the `azure-devops` extension. Install both
and authenticate:

```sh
# macOS
brew install azure-cli

# Linux / Windows
# see https://learn.microsoft.com/en-us/cli/azure/install-azure-cli

az extension add --name azure-devops

# Authenticate with a Personal Access Token (Code: Read & Write, Pull Request
# Threads, Build: Read). Either run `az devops login` and paste the PAT, or
# export it for non-interactive use:
export AZURE_DEVOPS_EXT_PAT=your-pat
```

Create a PAT from **User settings → Personal access tokens** in your Azure
DevOps organization. The daemon inherits `AZURE_DEVOPS_EXT_PAT` from the
environment it runs under, the same way the GitHub backend inherits `gh` auth.

Both `https://dev.azure.com/{org}/{project}/_git/{repo}` and the legacy
`https://{org}.visualstudio.com/{project}/_git/{repo}` remotes are detected, as
well as their SSH forms (`git@ssh.dev.azure.com:v3/...`).

**What you get:**

- PR creation and update (`az repos pr create` / `update`); Azure DevOps caps
  PR descriptions at 4000 characters, so the pipeline builds the body within
  that budget and applies a final truncation backstop with a visible marker.
  See the [PR step reference](/no-mistakes/reference/pipeline-steps/#pr) for
  section composition and truncation behavior.
- CI status polling - Azure branch policy evaluations (build validation and
  status checks) are read via `az repos pr policy list` until the PR is
  completed, abandoned, or the configured `ci_timeout` idle window elapses
- Merge-conflict polling and auto-fix from the PR's `mergeStatus`

**What you don't get (yet):**

- Failed check log fetching for the CI auto-fix step (the `az` CLI has no
  first-class build-log command)
- Fork PR routing (same as GitLab, Forgejo, and Bitbucket)

## Self-hosted GitHub/GitLab

Self-hosted GitHub Enterprise and self-hosted GitLab instances work through the same `gh` and `glab` CLIs. Authenticate the CLI against your instance (`gh auth login --hostname your-ghe.example.com`, `glab auth login --hostname gitlab.example.com`) and `no-mistakes` will route through the CLI as usual.

### Self-hosted GitHub Enterprise

GitHub Enterprise Server is detected the same way `github.com` is, as long as the host is one `gh` is authenticated against.
When the upstream hostname is not `github.com`, `no-mistakes` consults gh's configured hosts (`hosts.yml`, honoring `GH_CONFIG_DIR` then `XDG_CONFIG_HOME/gh`, then `~/.config/gh`) and treats the upstream as GitHub if its host appears there.
Running `gh auth login --hostname your-ghe.example.com` is enough to make detection succeed; if `gh` is not configured for the host, detection fails closed and the upstream is treated as unsupported.

On GHE, `gh --repo` expects a host-prefixed slug in the form `host/owner/name`.
`no-mistakes` builds that automatically from the recorded upstream remote or PR URL, so daemon-run `gh` commands resolve the right repository regardless of the daemon's working directory.
The fork owner extracted from the fork URL keeps the plain `owner/name` form because that side only feeds `--head owner:branch`.

### Self-hosted GitLab

Self-hosted GitLab is detected out of the box even when the hostname carries no `gitlab` marker (for example `git.example.com`).
When the hostname is not obviously GitLab, `no-mistakes` consults glab's configured hosts (`config.yml`, honoring `GLAB_CONFIG_DIR` then `XDG_CONFIG_HOME/glab-cli`, then `~/.config/glab-cli`) and treats the upstream as GitLab if its host appears there as a configured host or `api_host`.
Running `glab auth login --hostname your-gitlab.example.com` is enough to make detection succeed; if glab is not configured for the host, detection fails closed and the upstream is treated as unsupported.

The GitLab backend is pinned against `glab v1.5x`. Self-hosted detection and the merge-request and CI steps rely on its current flag and API surface, so keep `glab` reasonably up to date.

## SSH host aliases

SSH remotes that use a host alias from your SSH configuration (for example `git@github-personal:owner/repo` or `git@gitlab-work:group/repo`, where `github-personal`/`gitlab-work` map to a real `HostName` via `~/.ssh/config`) are supported. `no-mistakes` resolves the alias through `ssh -G` to its real host name and uses that host only for provider detection and identity checks, including scoping `gh` or `glab` to the right instance and matching an SSH Forgejo remote to `FORGEJO_BASE_URL`. The original Git remote URL is left untouched, so authentication and pushes continue to use the alias exactly as your SSH configuration expects.

If `ssh -G` is unavailable or the alias does not resolve, detection falls back to the literal host in the remote URL rather than failing the run.

## Unsupported hosts

If your upstream isn't GitHub, GitLab, Forgejo, Bitbucket Cloud, or Azure DevOps:

- The **push** step still runs - `no-mistakes` pushes through git to the configured target like any other remote.
- The **PR** step marks itself as `skipped`.
- The **CI** step marks itself as `skipped`.

Everything before push (rebase, review, test, document, lint) still works regardless of host. If your host has a CLI that exposes CI status and PR state, open an issue - new providers are straightforward to add.

## Checking what's wired up

```sh
no-mistakes doctor
```

`doctor` checks `gh` and `az` availability. For GitLab, confirm `glab` is installed and authenticated. For Forgejo, run `FORGEJO_BASE_URL=<host> forgejo-axi status --json` from the daemon's environment. For Bitbucket Cloud, confirm the two env vars are set in that environment. For Azure DevOps, confirm the `azure-devops` extension is installed (`az extension show --name azure-devops`) and a PAT is available.

:::note
When the daemon runs through a managed service (launchd, systemd, Task Scheduler), it reloads environment from your login shell on macOS and Linux so CLI auth and provider token variables are picked up, and it augments `PATH` with common binary directories. If credentials or PATH-derived tools are missing, check `~/.no-mistakes/logs/daemon.log` for a login-shell environment resolution warning. On Windows it reuses the current process environment.
:::
