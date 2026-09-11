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

JSON field names match `no-mistakes axi`. `GET /v1/axi/runs/{id}` returns
`{ "run": ... }`; ingest replies `201` with the same run object unwrapped. A run
carries `id`, `branch`, `status`, `head`, `pr`, `outcome`, `steps[]` (`step`,
`status`, `findings`, `items[]`) and, while a violation is open, `gate` plus
`help[]`. Finding objects (`steps[].items[]` and `gate.findings[]`) carry `id`,
`severity`, `file`, `line`, `action`, `class`, and `description`.

Acknowledgements are rendered on every later read: `responses[]` (`id`,
`action`, `created_at`) is the durable trail. The gate status stays
`awaiting_approval` after a response.
The violation is still open, so the gate and `outcome: failed` remain.

The verdict's `conclusion` is returned by `/respond` and `/notice`, and its
`portal_url` by `/notice`. The stored `public_summary` is the LAN copy of the
generic GitHub text and no route serves it.

LAN `description` values may include a snippet. The notice payload must not.

The portal never re-scans an ingest. The runner has already published its
verdict as the required GitHub check, so re-deriving one here would let an
independently versioned portal contradict a conclusion GitHub already
reported.

## Routes

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/v1/axi/home` | Recent firewall runs |
| GET | `/v1/axi/runs/{id}` | Axi-shaped status (LAN details) |
| GET | `/v1/axi/runs/{id}/logs` | Generic log lines only |
| POST | `/v1/axi/runs/{id}/respond` | `{ "action": "acknowledge" }` — does not pass the GitHub check |
| POST | `/v1/axi/runs/{id}/abort` | Cancels the LAN record: `run.outcome` becomes `cancelled` and the gate clears |
| POST | `/v1/firewall/verdicts` | Ingest from the self-hosted runner; the posted verdict is recorded verbatim |
| GET | `/v1/firewall/verdicts/{id}/notice` | Discord-safe payload |

Every `POST` mutates stored state and is gated by
`NO_MISTAKES_FIREWALL_INGEST_TOKEN` when the operator sets one. `GET` routes
are open on the LAN.

Ingest bodies are bounded at 8 MiB. An oversized body is answered `413`, never
truncated into a `400 invalid json`, so the runner can report a refused ingest
as an error instead of a Hard Rules violation.

Off-LAN notifiers, including Discord, MUST use `/notice`. That object has
`kind`, `repo`, `pr_url`, `portal_url`, and `conclusion` only.
