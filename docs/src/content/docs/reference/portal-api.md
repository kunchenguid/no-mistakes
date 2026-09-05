---
title: Portal API
description: LAN HTTP contract the firstmate portal consumes for firewall verdicts.
---

`no-mistakes firewall serve` binds a loopback address by default
(`127.0.0.1:8787`) and stores records in `$NM_HOME/firewall.sqlite`. It does
not start or stop the pipeline daemon. Cluster manifests bind `0.0.0.0:8787`
behind a ClusterIP Service.

firstmate-notify owns the LiveView. This repository owns the HTTP contract.

## Records

JSON field names match `no-mistakes axi`: `run.id`, `run.branch`, `run.status`,
`run.head`, `run.pr`, `steps[]` (`step`, `status`, `findings`), `findings[]`
(`id`, `severity`, `file`, `action`, `description`), plus firewall fields
`conclusion`, `public_summary`, `portal_url`, and `class_counts`.

LAN `description` values may include a snippet. Public and notice payloads
must not.

## Routes

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/v1/axi/home` | Recent firewall runs |
| GET | `/v1/axi/runs/{id}` | Axi-shaped status (LAN details) |
| GET | `/v1/axi/runs/{id}/logs` | Generic log lines only |
| POST | `/v1/axi/runs/{id}/respond` | `{ "action": "acknowledge" }` — does not pass the GitHub check |
| POST | `/v1/axi/runs/{id}/abort` | Cancels the LAN record: `run.outcome` becomes `cancelled` and the gate clears |
| POST | `/v1/firewall/verdicts` | Ingest from the self-hosted runner |
| GET | `/v1/firewall/verdicts/{id}/public` | Generic summary + class counts |
| GET | `/v1/firewall/verdicts/{id}/notice` | Discord-safe payload |

Off-LAN notifiers, including Discord, MUST use `/notice`. That object has
`kind`, `repo`, `pr_url`, `portal_url`, and `conclusion` only.
