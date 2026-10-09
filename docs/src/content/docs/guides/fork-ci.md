---
title: Fork CI on king-server
description: Local Linux validation and the required repository execution policy.
---

The `vifar/no-mistakes` fork runs its `CI` workflow on `king-server` through
the existing `ci-pilot` Linux virtual machine. The repository-scoped runner
`no-mistakes-king-server-1` uses the labels `self-hosted`, `Linux`, `X64`,
`king-server`, and `no-mistakes`. All three CI jobs require every label.
There is no hosted fallback when this runner is offline.

CI runs on pull requests from branches in this repository, pushes to `main`,
and manual dispatches. It checks formatting, the generated skill, and vet;
runs the full Go race suite; builds the Linux CLI; cross-compiles the other
desktop targets; and runs end-to-end journeys through `make e2e`. Go 1.27.1
avoids the race-enabled subprocess issue documented for Go 1.26.0–1.26.4.
Runtime tests run on Linux. Cross-compilation does not prove macOS or Windows
runtime behavior.

The runner needs Git, Make, a C compiler with libc development headers (for
the race detector), and the `sqlite3` CLI used by the Pi-profile journeys.
Administrators install these dependencies; the runner account does not gain
sudo access. The 104-journey end-to-end package has a 25-minute deadline inside
the 30-minute job bound. Each run retains the temporary-daemon inventory and
cleanup trap.

## Required execution policy

This public fork must enforce `.github/ci-actor-policy.json` as an **active,
repository-wide** GitHub Actions policy before any persistent local runner
is registered. It allows only `vifar` and the existing official
`github-actions[bot]`, `dependabot[bot]`, and `release-please[bot]` identities.
There is no workflow-path restriction: the policy applies to every workflow.

A job-level same-repository guard is an additional check. It cannot replace
the policy, because a contributor could change another workflow's runner
labels in a pull request. Do not automatically approve or rerun external
contributor workflows. Owner-triggered execution of external code requires
explicit authorization and review of the code and workflow first.

An administrator can apply the policy through GitHub's repository Actions
policies API, using the JSON file as the request body. Read it back and verify
the active enforcement, complete actor list, and absence of path exclusions.
Keep the runner stopped if that policy is removed or weakened. Policy changes
are administrative changes; editing the JSON file alone does not apply them.

See [GitHub's workflow execution controls](https://docs.github.com/en/actions/how-tos/administer/control-workflow-execution).

## Runner ownership and recovery

The runner runs as `nm-ci`, with a private home directory at `/home/nm-ci` and
its own workspace under `/home/nm-ci/actions-runner`. This account has neither
sudo access nor Docker-group membership. Its service is
`actions.runner.vifar-no-mistakes.no-mistakes-king-server-1.service`.
Existing Controller runners keep their separate registrations and services.

If CI queues, inspect this runner's status on the repository Actions runners
page and the dedicated service in `ci-pilot`. Recheck the policy before
starting or re-registering it. Use GitHub's official runner distribution,
verify its published SHA-256 digest, and pass a temporary registration token
through a protected channel. Never save that bootstrap token in this repo.
Runner credentials remain restricted to the runner account.

The upstream docs, Nix, generated-file, signature, and release workflows retain
their existing routing. This local workflow restores Go build and test CI;
it does not change publication destinations or install the CLI on the owner’s Mac.
