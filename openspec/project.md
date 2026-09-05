# Project Context

## Purpose

`no-mistakes` is a local Git proxy that validates a branch (review, test, docs, lint, push, PR, CI) before it reaches the configured push target. This checkout is a **detached fork** of `kunchenguid/no-mistakes`: local product work lives here, upstream is pulled, and pull requests are never sent upstream.

This fork additionally ships a **GitHub publish firewall**: a required check for configured public product repositories that blocks publishing live-system data (the ServiceRadar Hard Rules classes) without making other developer workstations install no-mistakes.

## Tech Stack

- Go 1.25+ CLI (`cmd/no-mistakes`), SQLite (`modernc.org/sqlite`), Cobra
- GitHub Actions composite actions and required checks
- Kubernetes manifests for a `no-mistakes` namespace (self-hosted Actions runner + LAN portal API)
- Docs site: Astro Starlight under `docs/`
- OpenSpec for change proposals

## Project Conventions

### Code Style

`gofmt -w .`. `make lint` is `go vet` plus generated-skill drift. Package comments own rationale for traps (see `AGENTS.md`).

### Architecture Patterns

- Pipeline daemon stays on the operator Mac at `~/.no-mistakes` and is not relocated for the firewall.
- Publish firewall is a separate process and SQLite file (`firewall.sqlite`) so a cluster namespace can run it without touching the Mac daemon.
- Public GitHub surfaces are generic; match details stay on the LAN portal API.
- Prefer GitHub required checks over a second git forge.

### Testing Strategy

- Unit tests next to the code (`go test -race ./...`).
- Pipeline e2e is behind the `e2e` build tag (`make e2e`).
- Action tests execute the real entrypoint the way a runner does, same pattern as `require_no_mistakes_action_test.go`.
- Fixtures are synthetic. Never commit captured live-system values.

### Git Workflow

- `origin` = this fork (`mfreeman451/no-mistakes`)
- `upstream` = `kunchenguid/no-mistakes`
- Feature branches from `origin/main`; PRs target `origin` only
- Sync: fetch `upstream` and merge (or rebase) onto the fork default branch locally; never open a PR against upstream

## Domain Context

Publish-policy classes are structural (IPs, MACs, emails, capture artifacts, …), not an organization-name word list. Unanchored abbreviations over-match ordinary English. Downstream registries cannot recall a publish.

## Important Constraints

- Fail closed: scanner errors, missing diffs, and failed portal ingest fail the GitHub check.
- Public check text, Discord/public notices, PR titles, and commit messages must never include matching snippets or customer names.
- Do not name customers in git, OpenSpec, PRs, check logs, or Discord.
- Do not put the firewall on a public VIP unless GitHub must reach a webhook. Self-hosted runners pull jobs outbound; no inbound webhook is required.

## External Dependencies

- GitHub Checks / Actions (required check on configured public repos; first: `carverauto/serviceradar`)
- Cluster namespace `no-mistakes` on the operator cluster
- firstmate-notify LiveView consumes the portal API (not implemented in this repo)
