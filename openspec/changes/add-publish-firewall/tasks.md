## 1. OpenSpec and fork docs

- [x] 1.1 Fill `openspec/project.md` and add change `add-publish-firewall`
- [x] 1.2 Document detached-fork workflow (origin, upstream, PRs to origin only, sync)

## 2. Scanner

- [x] 2.1 Implement anchored Hard Rules classifiers in `internal/firewall`
- [x] 2.2 Public summary never includes snippets, filenames, hostnames, IPs, or names
- [x] 2.3 Tests: allow documentation ranges; flag RFC1918/public IPs, MACs, emails, captures; do not match ordinary English as org abbreviations

## 3. Portal contract

- [x] 3.1 SQLite store for axi-shaped run/step/finding/respond plus firewall verdicts
- [x] 3.2 HTTP API: home/status/logs/respond/abort + verdict ingest/notice
- [x] 3.3 Public and notice payloads omit match content; LAN findings keep details
- [x] 3.4 `respond` acknowledges only; does not pass the GitHub check

## 4. CLI and GitHub action

- [x] 4.1 `no-mistakes firewall` serve/github-check and `no-mistakes axi firewall`
- [x] 4.2 Composite action + reusable workflow on self-hosted `no-mistakes` runners
- [x] 4.3 Action tests: generic public output, fail closed, no snippet leak

## 5. Cluster and operator docs

- [x] 5.1 Namespace `no-mistakes` manifests (runner + ClusterIP portal, no public Ingress)
- [x] 5.2 Docs for the firewall, portal API, environment variables
- [x] 5.3 AGENTS.md pointer; do not relocate the Mac daemon
