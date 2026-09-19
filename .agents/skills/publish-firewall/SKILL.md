---
name: publish-firewall
description: Use when changing the GitHub publish-policy check, Hard Rules scanners, LAN portal API, or cluster namespace no-mistakes.
user-invocable: false
metadata:
  internal: true
---

**Publish firewall**

- Detectors are structural and anchored in `internal/firewall`. Do not add an organization-abbreviation dictionary. Ordinary English must not match. Documentation ranges are allowlisted; RFC1918 and other non-documentation values fail closed.
- Public GitHub check text, job logs, Discord notices, commit messages, and PR titles must never include match snippets, filenames, hostnames, IPs, or names. `PublicText` and `Notice` own that boundary.
- Scan added and removed diff lines, filenames, titles, commits, and PR bodies. Deleting a captured value still publishes it.
- The action runs on self-hosted runners labeled `no-mistakes`. No public Ingress/VIP/webhook. The Mac daemon is not this process.
- Portal ingest uses `$NM_HOME/firewall.sqlite` and `no-mistakes firewall serve`. `axi firewall respond` acknowledges only; it does not green the GitHub check.
- Pin the composite action at a commit SHA of this fork, never `@main`.
- Do not name customers in git, OpenSpec, PRs, check logs, or Discord.
- Regressions: `internal/firewall/*_test.go`, `internal/cli/firewall_test.go`, `publish_firewall_action_test.go`.
