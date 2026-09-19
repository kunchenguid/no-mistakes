## ADDED Requirements

### Requirement: Axi-shaped records

The LAN portal API SHALL expose run, step, finding, and respond records using the same field names as `no-mistakes axi` (id, branch, status, head, pr, steps, findings, action, description) with route-specific verdict fields as documented in the [Portal API](../../../../../docs/src/content/docs/reference/portal-api.md#records).

#### Scenario: Status looks like axi
- **WHEN** a client GET `/v1/axi/runs/{id}` for a firewall scan
- **THEN** the JSON includes `run.id`, `run.branch`, `run.status`, `run.head`, `run.steps`, and finding items with `id`, `severity`, `file`, `action`, and `description`

### Requirement: LAN details vs public notice

LAN finding descriptions MAY include file, line, class, and snippet. Public verdict and notice payloads MUST NOT include snippets, filenames, hostnames, IPs, names, or customer identifiers. Discord and other off-LAN notifiers MUST use the notice payload.

#### Scenario: Notice has no match text
- **WHEN** a verdict has a finding whose description includes a matched address
- **THEN** GET `/v1/firewall/verdicts/{id}/notice` omits that address and any snippet

#### Scenario: LAN status keeps details
- **WHEN** the same verdict is fetched from GET `/v1/axi/runs/{id}`
- **THEN** the finding description still includes the class and location needed to fix the diff

### Requirement: The portal records the published verdict verbatim

Ingest SHALL store the conclusion the client already published to GitHub. The portal MUST NOT re-scan an ingested pull request, so an independently versioned portal can never contradict the required check.

#### Scenario: A clean ingest stays clean
- **WHEN** the runner posts a clean verdict whose diff still contains a live-system value
- **THEN** the stored conclusion is `success`, matching the check GitHub reported

### Requirement: Respond does not green the check

POST `/v1/axi/runs/{id}/respond` SHALL record an acknowledgement. It MUST NOT change the GitHub check conclusion. A successful check run is required to pass.

#### Scenario: Acknowledge leaves conclusion failed
- **WHEN** an operator responds with action `acknowledge` on a failing verdict
- **THEN** the stored conclusion remains `failure` and a response row is recorded

#### Scenario: A later reader can see the verdict was reviewed
- **WHEN** a second operator GETs `/v1/axi/runs/{id}` after an acknowledgement
- **THEN** the run renders `responses[]` and its gate status stays `awaiting_approval`, while the gate and `outcome: failed` remain because the violation is still open

### Requirement: An ingest the portal refuses is not a verdict

The portal SHALL bound the ingest body and answer an oversized request `413` rather than truncating it into a malformed document. The runner SHALL report a refused or unreachable portal as an `error` conclusion, never as a Hard Rules `failure`.

#### Scenario: An oversized ingest is rejected as too large
- **WHEN** the runner posts a well-formed ingest for a large but clean pull request that exceeds the body limit
- **THEN** the portal answers `413`, stores no verdict, and the check concludes `error` rather than reporting a publish-policy violation on a clean diff

### Requirement: Mutating portal routes are authorized

When an ingest token is configured, every portal request that mutates stored verdict state (ingest, respond, abort) SHALL require it. Read routes stay open on the LAN.

#### Scenario: Unauthenticated abort cannot cancel a live violation
- **WHEN** a token is configured and a caller POSTs to `/v1/axi/runs/{id}/abort` or `/respond` without it
- **THEN** the portal answers 401, the verdict keeps its `awaiting_approval` gate, and no acknowledgement row is recorded

### Requirement: Firewall serve is not the Mac daemon

`no-mistakes firewall serve` SHALL bind a configurable listen address (default loopback) and use `$NM_HOME/firewall.sqlite`. It MUST NOT start, stop, or relocate the pipeline daemon.

#### Scenario: Default listen is loopback
- **WHEN** `firewall serve` is started with no listen flag
- **THEN** it listens on `127.0.0.1` and opens `firewall.sqlite` under `NM_HOME`
