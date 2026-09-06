## Context

This checkout is a detached fork. Product work (the publish firewall) must land here, still pull `kunchenguid/no-mistakes`, and never open PRs against upstream.

The Hard Rules for public product source: never commit data captured from a live system (production, staging, lab, demo, customer or partner). Classes are structural. Stripping an organization name is not enough if the fingerprint remains. Comments, docs, and commit/PR titles count. Downstream registries cannot recall a publish.

A required GitHub check can enforce this without a second forge. GitHub Actions logs on a public repository are public, so the check text must not echo matches.

## Goals / Non-Goals

- Goals:
  - Required GitHub check on configured public repos (first: `carverauto/serviceradar`; SDK and others via config)
  - Anchored structural scanners (no org-abbreviation word list)
  - Generic public check output + LAN portal URL
  - Cluster namespace `no-mistakes` for the check runner and LAN portal
  - Portal HTTP API axi-shaped records + firewall verdicts
  - Detached-fork docs
- Non-Goals:
  - Relocating the Mac `~/.no-mistakes` daemon
  - Implementing firstmate-notify LiveView
  - Discord bots in this repo (only a public-notice payload the notifier may send)
  - Opening PRs against upstream
  - A second git forge
  - Customer-name lists in git

## Decisions

- Decision: GitHub Actions on **self-hosted runners** in namespace `no-mistakes`, not GitHub-hosted runners and not a public webhook.
  - Runners connect outbound to GitHub to pull jobs. GitHub does not need to reach the LAN. No public VIP.
  - The scanner binary lives on the runner. Stdout/stderr uploaded to GitHub contain only generic text.
  - Alternative considered: GitHub-hosted `ubuntu-latest` — rejected because job logs and workspaces leave the LAN and the portal is unreachable without a public ingest VIP.
  - Alternative considered: GitHub App inbound webhook on a public VIP — only if required checks cannot run on self-hosted runners; they can.

- Decision: Scan coverage follows the [publish-firewall requirement](specs/publish-firewall/spec.md#requirement-surfaces-that-count), including introduced commit history and live PR metadata.

- Decision: Detectors are **structural and anchored**. No organization-abbreviation dictionary. Documentation ranges (RFC 5737/3849/2606, IANA TEST-NET MAC `00:00:5e:00:53:00/24`, `555-01xx`, `example.com`/`example.net`/`example.org`, `SITE01` / `host01.example.com`) are allowlisted. RFC1918 and other non-documentation addresses fail closed.

- Decision: Firewall state is a **separate SQLite file** (`$NM_HOME/firewall.sqlite`) and a **separate `firewall serve` process**. Cluster `NM_HOME` is not the Mac daemon root.

- Decision: Public GitHub output is `publish-policy ok` on success, `publish-policy violation` for findings, or `publish-policy error` for scanner or portal failures, plus an optional LAN portal URL. Discord must use the notice object documented in the [Portal API](../../../docs/src/content/docs/reference/portal-api.md), never finding descriptions.

- Decision: Portal ingest failure fails the check (fail closed). A clean scan is the only success path.

- Decision: `respond` on a firewall verdict records acknowledgement only. It does not green the GitHub check; a successful check run is required.

## Risks / Trade-offs

- Structural scanners miss some “real” values that look like prose (person names without emails). Mitigation: fail closed on high-confidence classes; titles and comments still go through the same scanners; do not pretend name-dictionaries are safe.
- Self-hosted runner availability: if the runner is down, the required check does not start. Mitigation: fail closed at the ruleset (missing check blocks merge).
- Portal URL in public check text is LAN-only; GitHub users off-LAN cannot open it. That is intended.

## Migration Plan

1. Land scanner, action, portal API, cluster manifests, and docs on this fork.
2. Deploy namespace `no-mistakes` and a labeled self-hosted runner.
3. Configure `carverauto/serviceradar` to call the action pinned at a commit SHA (never `@main`) as a required check.
4. Add SDK and other public product repos by the same pin + ruleset.

Rollback: remove the required check from the product repo ruleset; this fork keeps the code.

## Open Questions

None for this change. Product-repo ruleset installation is an operator step outside this repository.
