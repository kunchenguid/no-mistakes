# Change: Add a GitHub publish firewall on a detached no-mistakes fork

## Why

Public product repositories cannot recall a publish. Live-system values (hostnames, addresses, captures, identities, fleet figures) must not land in git, GitHub check logs, Discord, or commit/PR titles. Other developer workstations should not have to install no-mistakes for that gate. This detached fork is the place to add that firewall without sending PRs upstream.

## What Changes

- Document the detached-fork workflow: `origin` is this fork, `upstream` is `kunchenguid/no-mistakes`, PRs target `origin` only, sync from upstream.
- Add a publish-firewall scanner and GitHub required-check action that scans the PR diff (and titles/commit messages/PR body) against the Hard Rules classes, fails closed, and emits only generic public text plus a LAN portal URL.
- Run the check on self-hosted runners in cluster namespace `no-mistakes` so GitHub never needs an inbound webhook or public VIP.
- Add a LAN portal HTTP API of axi-shaped run/step/finding/respond records plus firewall verdicts, for firstmate-notify to consume. Discord/public notices carry no match content.
- Keep the Mac `~/.no-mistakes` daemon where it is.

## Impact

- Affected specs: `publish-firewall`, `detached-fork`, `portal-contract` (new)
- Affected code: `internal/firewall`, `internal/cli`, `internal/paths`, `.github/actions/publish-firewall`, `deploy/no-mistakes`, docs, `AGENTS.md`
