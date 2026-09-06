# `publish-firewall`

Composite action that scans a pull request for live-system values that must
not be published. It is the reusable implementation of the required check
named **`publish-policy`**.

Public GitHub logs emit `publish-policy ok` on success, `publish-policy violation` for findings,
or `publish-policy error` for scanner or portal failures, with an optional LAN
portal URL. Match snippets, filenames, hostnames, IPs, and names never appear
in check text, annotations, Discord notices, commit messages, or PR titles
produced by this action.

Run it on **self-hosted runners** in cluster namespace `no-mistakes`. Those
runners pull jobs outbound from GitHub, so the firewall does not need a
public webhook or VIP. GitHub-hosted runners would upload workspace and logs
off the LAN; do not use them for this check.

Pin a commit SHA of this fork (`mfreeman451/no-mistakes`). Never `@main`:
`main` is editable by a pull request that could rewrite its own judge.

```yaml
name: Publish firewall
on:
  pull_request:
    types: [opened, synchronize, reopened, edited]

permissions:
  contents: read
  pull-requests: read

jobs:
  publish-policy:
    name: publish-policy
    runs-on: [self-hosted, no-mistakes]
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: mfreeman451/no-mistakes/.github/actions/publish-firewall@<commit-sha>
        with:
          portal-url: http://no-mistakes-portal.no-mistakes.svc.cluster.local:8787
```

The first configured public product repository is `carverauto/serviceradar`.
Add others by pinning this action and requiring the `publish-policy` check in
the repository ruleset.

## Inputs

| Input | Default | Purpose |
| --- | --- | --- |
| `portal-url` | `""` | LAN portal base URL for ingest. When set, ingest failure fails the check. |
| `portal-token` | `""` | Optional bearer token for ingest. |
| `github-token` | `${{ github.token }}` | Read the PR. |
| `repo` | `${{ github.repository }}` | owner/name |
| `pr-number` | event payload | Pull request number |

A missing `no-mistakes` binary, a missing diff, or a scanner error fails
closed with the same generic public phrase.
