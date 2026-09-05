## ADDED Requirements

### Requirement: GitHub required check for publish policy

The system SHALL provide a reusable GitHub composite action that scans a pull request against the publish-policy classes and reports a required check. Configured public product repositories SHALL be able to pin that action at a commit SHA (never a mutable branch). The first configured repository is `carverauto/serviceradar`; additional repositories SHALL be listed in cluster config without code changes to the scanner.

#### Scenario: Clean synthetic diff passes
- **WHEN** a pull request diff contains only documentation-range addresses, `example.com` identities, and no capture artifacts
- **THEN** the check concludes success and public output does not mention a violation

#### Scenario: Live-system class fails closed
- **WHEN** a pull request diff, filename, title, commit message, or body contains a non-allowlisted value in a Hard Rules class
- **THEN** the check concludes failure

#### Scenario: Scanner error fails closed
- **WHEN** the diff cannot be read, the scanner errors, or portal ingest is configured and fails
- **THEN** the check concludes failure and MUST NOT conclude success

### Requirement: Generic public check text

Public GitHub check output, annotations, and job logs SHALL contain only the generic phrase `publish-policy violation` and an optional LAN portal URL. They MUST NOT echo matched hostnames, IPs, MACs, names, emails, snippets, filenames, or PR titles.

#### Scenario: Violation output is generic
- **WHEN** the scanner finds a non-documentation IPv4 address in the diff
- **THEN** public stdout contains `publish-policy violation` and does not contain that address

### Requirement: Anchored structural search

Pattern search SHALL use word boundaries or a qualifying delimiter. The scanner MUST NOT use an organization-abbreviation dictionary. Ordinary English such as `equal`, `manual`, `actual`, `virtual`, and `quality` MUST NOT match as a policy hit.

#### Scenario: Ordinary English is not a hit
- **WHEN** the diff contains the words `equal`, `manual`, and `actual`
- **THEN** the scanner reports no publish-policy findings from those words

#### Scenario: Documentation ranges are allowed
- **WHEN** the diff contains `192.0.2.1`, `2001:db8::1`, `user@example.com`, and `555-0100`
- **THEN** the scanner reports no findings for those values

#### Scenario: Ordinary configuration values are not live identifiers
- **WHEN** the diff contains `namespace: no-mistakes`, `cluster: staging`, `build: true`, or `vlan: 100`
- **THEN** the scanner reports no findings, because the keyword alone is not a hit and none of those values carries an instance identity

#### Scenario: An opaque token without digits is still a capture
- **WHEN** the diff contains `jsessionid=ABCDEFGHIJKLMNOP` or `serial: ABCDEFGH`
- **THEN** the scanner reports a finding: the value gate dismisses only recognisable code, so a token that happens to carry no digit still fails closed

#### Scenario: Tracing plumbing and port counts are not captures
- **WHEN** the diff contains `trace_id: req.TraceID`, `serialNumber: row.SerialNumber`, or `scans up to 65535 ports per host`
- **THEN** the scanner reports no findings, because a session or serial value must be an opaque token and a port count is not a fleet unit

#### Scenario: Coordinates split across lines still fail
- **WHEN** a diff serializes `"latitude": 37.774929,` and `"longitude": -122.419416` on separate lines
- **THEN** the scanner reports a GPS finding, and an ordinary float with no lat/lon keyword nearby reports none

### Requirement: Surfaces that count

The scanner SHALL inspect added and removed unified-diff lines, changed filenames, the pull request title, commit messages, and the pull request body.

#### Scenario: Removed live value still fails
- **WHEN** a pull request only deletes a non-documentation IP from a file
- **THEN** the check concludes failure because the public diff still contains the value

### Requirement: Self-hosted cluster runner without public VIP

The check SHALL be designed to run on self-hosted GitHub Actions runners in Kubernetes namespace `no-mistakes`. The design MUST NOT require GitHub to reach a public webhook. The Mac `~/.no-mistakes` daemon SHALL remain on the operator workstation.

#### Scenario: No inbound webhook
- **WHEN** an operator deploys the provided manifests
- **THEN** the portal Service is ClusterIP-only and no Ingress or public VIP is defined for the firewall
