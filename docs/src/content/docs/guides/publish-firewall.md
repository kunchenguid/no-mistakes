---
title: Publish firewall
description: Required GitHub check that blocks publishing live-system data.
---

The publish firewall is a required GitHub check for configured public product
repositories. It enforces the Hard Rules classes: never commit data captured
from a live system, and never let a real value become a fixture. That applies
to production, staging, lab, demo, and any customer or partner environment.

Other developer workstations do not install no-mistakes. The check runs on
self-hosted runners in cluster namespace `no-mistakes`. See
[cluster notes](https://github.com/mfreeman451/no-mistakes/tree/main/deploy/no-mistakes).

## What is scanned

The action scans added **and** removed lines in the aggregate pull request diff
and every introduced commit’s content changes, plus filenames, commit messages,
and the current PR title and body fetched from GitHub. An add-then-remove
sequence still exposes the value in history; a cleanup PR still exposes it
in the public diff. Both fail. Metadata lookup failures and binary content
omitted by Git fail closed as scanner errors, not Hard Rules findings.

Detectors are structural and **anchored**. There is no organization-abbreviation
list. Ordinary English (`equal`, `manual`, `actual`) is not a hit.
Documentation ranges (`192.0.2.0/24`, `example.com`, `555-0100`, IANA
documentation MACs, `SITE01`) are allowed. RFC1918 and other non-documentation
addresses fail closed. CIDRs must fit entirely within an allowed range.
IPv4-mapped IPv6 addresses use the IPv4 allowlist; embedded IPv4 or MAC-shaped
substrings of a complete IPv6 literal are not judged as separate addresses.

Keyword-anchored classes (k8s identifiers, network policy names, serials,
session and trace IDs, firmware builds, GPS) match on the assigned value, not
the keyword. `namespace: staging` and `serialNumber: row.SerialNumber` are
configuration and code; `namespace: prod-tenant-a`, `serial: SN9F3K21AB`, and
`firmware: 17.9.4a` are captured values.

Site and facility codes may be alphabetic: `airport-lhr`, `facility-west12`,
and `dc-east-01` are hits while the bare schema and package
identifiers `site_id`, `site_name`, `dc_name`, and `site-packages` are not.

## Public vs LAN

| Surface | Content |
| --- | --- |
| GitHub check / job logs | `publish-policy ok` on success; `publish-policy violation` for findings or `publish-policy error` for scanner or portal failures, with an optional LAN portal URL |
| Discord / off-LAN notice | `GET /v1/firewall/verdicts/{id}/notice` — no snippets |
| LAN portal | axi-shaped run/step/finding records with file, line, class, snippet |

Commit messages and PR titles must describe the change by class, not by the
matched value. Downstream registries cannot recall a publish.

## Product repositories

The first configured public repo is `carverauto/serviceradar`. Add SDK and
others in `deploy/no-mistakes/config.yaml` and require the check named
`publish-policy` in that repository's ruleset. Pin the composite action at a
commit SHA of this fork, never `@main`.

Usage: [`.github/actions/publish-firewall/README.md`](https://github.com/mfreeman451/no-mistakes/blob/main/.github/actions/publish-firewall/README.md).

The Mac `~/.no-mistakes` daemon is not this check and is not relocated.

## CLI

```sh
no-mistakes firewall github-check --diff pr.diff
no-mistakes firewall serve --listen 127.0.0.1:8787
no-mistakes axi firewall status --run <id>
no-mistakes axi firewall respond --run <id> --action acknowledge
```

`respond` records that an operator saw the verdict. It does not green the
GitHub check. A successful check run is required to pass. A rerun can pass
after correcting PR metadata or restoring portal availability without a new
head SHA, provided all scanned surfaces are clean.

Portal HTTP contract: [Portal API](/no-mistakes/reference/portal-api/).
Environment variables: [Environment](/no-mistakes/reference/environment/).
