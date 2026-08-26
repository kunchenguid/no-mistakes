# Cached Repository State Design

## Intent

Rebuild the legitimate portion of the old fork's diagnostics intent on current
`upstream/main`: make `no-mistakes status` always show the already-available
local branch evidence that an agent needs before starting work.

## Scope

`status` will call the existing `branchsync.Service.InspectCached` once and
render a clearly labelled cached summary containing the local branch, a short
HEAD, clean/dirty state, any local reason, and the existing human branch-sync
summary. Its telemetry fingerprint will include that rendered evidence so a
meaningful displayed state change is observable by the sampled read surface.

## Safety contract

`InspectCached` is the data source because it explicitly does not fetch,
contact a remote, alter refs, alter the index, alter the worktree, create a
pipeline run, or mutate the database. The output must say `cached`; it must
not claim current remote freshness.

## Non-goals

- No new pipeline step, daemon behaviour, schema, gate, worktree inventory,
  remote inspection, or direct process execution.
- No recovery or synchronization action from `status`.
- No recovery of the fork's unregistered pipeline experiment or tracked binary.

## Acceptance criteria

1. A registered repository's `status` output always includes cached local
   branch evidence, including an explicit unavailable form when Git evidence is
   absent.
2. A clean and a dirty local state render distinguishable, actionable text.
3. A change to rendered cached state changes the status telemetry fingerprint.
4. Existing status behaviour for repo identity, daemon, and active-run display
   remains intact.
5. `gofmt -w .`, `make lint`, `go test -race ./...`, and
   `go build -o ./bin/no-mistakes ./cmd/no-mistakes` succeed before review.
