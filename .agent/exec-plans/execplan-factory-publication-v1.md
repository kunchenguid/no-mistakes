<!-- execplan_template_version: 4 -->
---
execplanTemplateVersion: 4
deliveryShape: cross-repository
---

# ExecPlan — Factory publication protocol v1

## Purpose / Big Picture

Add a fail-closed publication-only mode to No Mistakes. Agent Factory hands the
mode a completed Protected build-loop candidate; No Mistakes then runs its
existing defense-in-depth steps without changing that candidate, parks before
each Push and PR effect for a separately bound Owner decision, publishes the
exact candidate commit, and observes non-empty CI for that exact commit.

This is not a second build loop. Agent Factory remains authoritative for
Build, Check, Review, DoD, P4, resume, and the final candidate. No Mistakes
keeps one Run aggregate, one Executor, and the existing nine step names. The
new `factory-publication-v1` run kind is a downstream publication profile of
that machinery. Publication effects use a crash journal; those effect records
do not constitute another SDLC state machine.

### Current implementation gate

The protocol, persistence, one-Executor composition, exact candidate/effect
bindings, crash recovery, and offline core journey are implemented and tested.
The production profile is not release-ready: independent current-byte review
proved that same-UID mode bits and prompt rules do not technically confine the
defense agent or configured commands. Until a real inherited filesystem,
credential, and egress boundary plus an authenticated model-provider broker is
available, production admission must stop before the first defense process with
`confinement_unavailable`. The offline test-only boundary is not production
confinement evidence.

### Owner GO — N0 Codex confinement

On 2026-08-30 the Owner selected the restricted mechanically-proved path:
implement `GO-N0-CONFINEMENT-CODEX`, initially for Codex only. This GO does
not authorize a new dependency, package extraction,
container runtime, sudo/host reconfiguration, Windows support, another agent,
a fallback agent, a live provider mutation, or a weaker unconfined path.

The initial closed support matrix is:

- agent: exact `codex` executable only;
- production platforms: `linux/arm64` and `linux/amd64`, each only after its
  own non-skipped capability and adversarial process tests pass on the current
  bytes;
- `darwin/arm64` is an explicit fail-closed negative platform for this slice.
  Its Seatbelt filesystem/network boundary passed, but a hostile `setsid`
  descendant could change to `/` and outlive wrapper cancellation. Adding a
  macOS process container or host service would cross this GO's no-dependency /
  no-host-reconfiguration boundary, so there is no macOS production fallback;
- every other agent, fallback, platform, unsupported Codex build, missing
  sandbox primitive, or failed capability probe: `confinement_unavailable`
  before candidate materialization, Run admission, or a defense process;
- sessions/resume are disabled for publication defense; every invocation is
  cold and ephemeral;
- configured Test/Lint commands cross the same technical boundary as agent
  tool commands. They may not fall back to direct `sh -c` execution.

The Owner principal is the authenticated same-user process outside the defense
sandbox. Push/PR decision digests are public challenges, not bearer authority.
The defense process must be unable to reach the daemon/Owner Unix socket,
start a nested authenticated Codex/no-mistakes client, inherit credentials, or
use TCP. Existing authenticated peer ancestry remains mandatory outside the
sandbox; no prompt, marker, CWD, or result text can authorize an effect.

The permitted no-dependency implementation uses the installed Codex permission
profile/sandbox primitives. One immutable in-process
`PublicationCodexBoundaryV1` value is the only policy owner. It contains the
complete ordered executable closure, exact native `codex --version`,
`GOOS/GOARCH`, canonical inline permission-profile bytes and digest, and the
canonical fixed-argument hashes for model-free probe, `codex exec`, and
configured-command launch. The closure records a role, realpath, raw-byte
SHA-256, mode, owner and file identity for every pre-sandbox executable: the logical
Codex entry point used for provenance; its interpreter only if it will actually
run; the selected platform-native Codex binary; a separate Linux sandbox helper
if the installed build uses one; and the exact system or bundled bubblewrap
binary selected for lifecycle confinement. The private canary executable and a
long-lived controller sentinel are also mandatory closure members; neither may
be resolved ad hoc after the capability probe.

The production boundary resolves the installed package once, then directly
executes the pinned native Codex binary. It never executes the JavaScript/npm
launcher or Node, and never asks `PATH` to rediscover Codex. Linux bootstrap
`PATH` is a closed value that can select only the already pinned bubblewrap (or
is empty to select the already pinned bundled helper); the sandboxed tool PATH
is supplied separately by the canonical permission/environment policy. If the
actual installed build can execute a helper that the boundary cannot enumerate
and pin, the platform remains `confinement_unavailable`.

No launch may resolve a named profile from mutable user/project configuration.
The same canonical profile value is rendered unchanged into all three launch
forms. Every closure member's realpath, raw bytes, mode, owner and file identity, plus
the native version, platform, policy digest and fixed arguments, are revalidated
immediately before every launch. A symlink, launcher, native-binary,
sandbox-helper or bubblewrap swap is `confinement_unavailable`, never a re-probe
against new bytes or a fallback.

A model-free `codex sandbox` canary built by that value must prove
candidate-read, scratch-write,
candidate/source/sibling/home denial, network and Unix-socket denial, sanitized
environment, and process cleanup before the production composition is made
available. The agent launch must ignore user/project settings, forbid raw
execution/sandbox overrides, disable web/MCP/connectors/browser/computer-use,
and keep Codex service/auth traffic outside the command sandbox without making
credentials visible to sandboxed commands.

Mac intake against `codex-cli 0.150.1` proved filesystem and network separation
non-vacuously: candidate read and scratch write succeeded; candidate write and
mode change, sibling read, loopback TCP, daemon-shaped Unix socket, and nested
`codex login status` were denied. It also falsified both ordinary process-group
cleanup and CWD-based ownership: a `setsid` child changed to `/`, survived
wrapper cancellation, and wrote late scratch state. Therefore
`internal/procreap` is explicitly not an authority for this boundary and its
ordinary best-effort behavior is unchanged.

Linux release credit instead requires the exact pinned Codex Linux bubblewrap
path to prove a new PID namespace, PID-1 reaping, and parent-death teardown.
Before Codex or the command payload can execute, the boundary arms bubblewrap's
fixed `--block-fd 3`/`--info-fd 4` barrier. The controller accepts the bounded
child-PID response only after exact PID/start/parent/namespace binding and an
`NSpid` chain whose first value is that host PID and whose final value is `1`;
only then does it release the payload. This proves the bound process is the new
namespace init. After wrapper wait, exact absence of that PID/start identity is
the lifecycle authority: Linux kills every remaining member when namespace
init exits, including nested-namespace and non-dumpable descendants, so a
global owner-based `/proc` scan is neither needed nor trusted. A separate
closure-bound, non-zombie/non-stopped sentinel must remain unchanged.
The model-free canary starts a real descendant which creates a new session,
changes CWD to `/`, closes inherited non-stdio descriptors, and attempts a late
scratch write after controller cancellation. The controller must positively
observe the child alive before cancellation, then prove bounded wrapper exit,
child absence by exact host PID/start identity, an empty namespace, and no late
marker. Unsupported namespace behavior is `confinement_unavailable`. Candidate
removal occurs only after the launch boundary has reported certain teardown. A
launch that cannot prove bounded teardown returns the distinguished cleanup
error, keeps the private candidate/scratch container for recovery evidence, and
makes the Publication Run non-success. No CWD scan, signal-only best effort, or
unbounded wait earns credit.

## Authority and Read Order

1. `AGENTS.md` owns repository, TDD, verification, security, skill-generation,
   and external-effect rules.
2. The Owner-approved product chain is: build-loop DONE -> No Mistakes
   publication gate -> Push/PR/CI -> merge-pr Exact-Head review -> Approval ->
   Merge.
3. `internal/types/types.go`, `internal/pipeline/executor.go`, and
   `internal/pipeline/steps/common.go` own the existing Run/Executor/step
   semantics. N0 extends them; it does not create another executor.
4. `internal/db/schema.go` and `internal/db/run.go` own durable Run state.
5. `internal/scm/host.go` and `internal/scm/github` own forge observations.
6. Base checkpoint:
   `ab2544298b745e9dc1a01fcf9a3151a247926083` (`v1.60.0`).
7. Independent plan gate: Senox `APPROVE` on 2026-08-29 after adding exact
   PR-reconcile and consumable Push/PR decision bindings.

acceptedCheckpoint: `ab2544298b745e9dc1a01fcf9a3151a247926083`

## Non-negotiable contract

### Public protocol

Expose a separate machine interface:

- `no-mistakes publication start`
- `no-mistakes publication authorize`
- `no-mistakes publication status`

The interface reads strict canonical JSON v1 from stdin or a named request
file and writes exactly one closed JSON v1 result to stdout. Human progress
goes to stderr. Unknown, missing, duplicate, malformed, non-canonical, or
trailing input is rejected. `publication_id` is the lowercase SHA-256 of the
canonical request bytes.

The canonical request binds at least:

- protocol;
- Factory run ID, terminal T10 sequence, and validated run-state prefix hash;
- PlanBinding hash;
- WorkContract repository path and raw-byte hash;
- a bounded build-intent projection;
- registered repository ID, full candidate branch ref, base ref, exact bound
  base commit `B`, exact candidate commit `H`, and candidate tree;
- publisher executable path, raw-byte hash, build SHA, and protocol version;
- closed Push, PR, and read-only CI scopes.

The WorkContract is read from commit `H` and checked byte-for-byte. No Mistakes
does not implement the Factory TOML parser. N1 derives the intent projection
from the authoritative Factory chain and binds it into this request.

The start request does not contain a caller-supplied publication ID; such a
field is unknown and refused. Parsing first proves canonical bytes and then
derives `publication_id = SHA-256(canonical_request_bytes)`. Result and later
authorize/status envelopes carry that derived ID. This avoids a circular
self-hash while keeping the complete request content-addressed.

Same derived publication ID plus identical canonical bytes reconciles the same Run.
The same ID plus different bytes, a different candidate, a concurrent Run for
the same repository/ref, or any attempt to attach an ordinary AXI Run is
refused.

### Existing Run and steps

Add a closed Run kind `standard|factory-publication-v1`. Ordinary AXI behavior
must remain unchanged. Publication mode uses the same ordered step names:

1. Intent validates the bound request.
2. Rebase is read-only freshness/up-to-date validation; a required rebase is
   failure.
3. Review, Test, Document, and Lint run fully as defense in depth.
4. Push, PR, and CI implement the closed publication contract below.

Defense steps execute against disposable, technically read-only candidate
views with writable scratch outside the candidate. Before and after each step,
the product verifies exact `HEAD == H`, exact tree, clean tracked/index/
untracked state, and unchanged refs/config/replace-refs. Publication mode never
fixes, retries, rebases, formats, stages, commits, skips, or converts a quality
failure to completed. Requested or observed mutation is `DRIFT` or `FAILED`.

### Push and PR Owner decisions

Push and PR are the only mutating external ports in N0 and each has its own
durable pre-effect Owner decision.

The Push decision binds exactly:

`publication_id + H + remote_identity + destination_ref + effect_digest`.

The PR decision additionally binds:

`base_ref + head_ref + draft_digest`.

Before the port is invoked, all fields are observed again. Any mismatch
consumes and invalidates the decision. Decisions cannot be transferred between
effects, candidates, attempts, or retries. After an effect may have begun, the
same decision can never authorize another invocation.

Push publishes only object `H` to the exact destination ref and then observes
that ref at exactly `H`.

The PR draft is completely rendered and redacted before its Owner gate. Its raw
bytes and digest are persisted. It includes a machine-readable
publication/effect marker as a reconciliation locator, never as a trust root.
Recovery accepts exactly one PR matching:

`repo_id + base_ref + head_ref + H + marker + draft_digest`.

Zero or multiple matches after a possible effect yields `EFFECT_UNKNOWN`.
Recovery never adopts another PR and never blindly creates a replacement.

### CI and result

CI is read-only observation after the PR effect; it does not need an Owner
decision and may not rerun, fix, commit, or push. The live PR head and every
check must bind to `H`. READY requires a non-empty set of fully passing checks.
Empty/no-CI, skipped, partial, pending, cancelled, failed, unknown, malformed,
or head-drifted observations are never green.

Only terminal `READY` returns success. `CHECKING`, `READY_FOR_PUSH`,
`READY_FOR_PR`, `CI_OBSERVING`, `FAILED`, `DRIFT`, `DENIED`, and
`EFFECT_UNKNOWN` do not.

### Persistence and recovery

Extend the existing Run with a run kind. Persist a one-to-one Publication
binding and a Push/PR/CI effect journal containing request bytes/digest,
decision digest, decision consumption, effect start, observation, and exact
bindings. These are orthogonal external-effect facts, not another SDLC
aggregate.

Publication creation and Run association are one transaction. Startup excludes
Publication Runs from ordinary stale-pipeline terminalization and resumes them
through publication reconciliation. Read-only defense work can be repeated on
fresh scratch. An authorized effect that may have started is always reconciled
before action. Ambiguity becomes `EFFECT_UNKNOWN`; no blind replay.

CLI and daemon must prove exact protocol/build/binary compatibility. A pinned
CLI may not silently use an incompatible existing daemon. Publication mode
does not perform update checks or telemetry outside its explicitly authorized
effect boundaries.

### Candidate admission and provider boundary

N0 v1 is GitHub-only. Other providers and fork routing fail closed. The candidate is admitted
through a registered repository ID and exact local full branch ref, `H`, and
tree into No Mistakes-owned disposable storage. The live base ref must still
equal the bound base commit `B`, `B` must be a commit and an ancestor of `H`;
multi-commit feature histories remain valid. `no-mistakes init` is never called
by this flow. Existing repository bootstrap remains an explicit operator
prerequisite.

Tests use local repositories, local bare remotes, fake forge/CI ports, and fake
agents. They perform no real network, authentication, Push, PR, CI, or provider
mutation.

## Program Integration Line Strategy

Implement on `codex/factory-publication-v1` in the isolated worktree
`codex-no-mistakes-n0`, based on the immutable checkpoint above. N0 is its own
No Mistakes change. N1 is a later Agent Factory change and may consume only a
reviewed, frozen N0 protocol.

The current `origin` points only to `kunchenguid/no-mistakes.git`; do not push
until the Owner provides an authorized writable remote. Local commits and
verification do not imply publication authority.

## Milestones

| Id | Outcome | Status |
|---|---|---|
| M0 | Exact base, repository rules, current failure modes, and Senox plan gate recorded | complete |
| M1 | Red tests freeze strict JSON, run-kind/schema, idempotent admission, and publisher identity | complete |
| M2 | Red tests freeze read-only defense behavior and ordinary AXI regression | in progress under GO-N0-CONFINEMENT-CODEX |
| M3 | Red tests freeze Push/PR decisions, exact reconcile, CI at H, and crash recovery | complete |
| M4 | Product implementation satisfies focused and adversarial tests | in progress under GO-N0-CONFINEMENT-CODEX |
| M5 | Formatting, lint, race, E2E, build, and manual falsifiers pass | core gates complete; Mac/Linux confinement gates pending |
| M6 | Exact-byte independent Senox review passes; branch push is either authorized or held | pending |

## Planned file boundary

### Exact `GO-N0-CONFINEMENT-CODEX` slice boundary

This subsection is the closed edit boundary for the active slice. A required
production or test file outside this list reopens the Owner scope gate before
editing.

Production:

- new `internal/agent/publication_boundary.go`
- new `internal/agent/publication_boundary_linux.go`
- new `internal/agent/publication_boundary_unsupported.go`
- `internal/agent/agent.go`
- `internal/agent/codex.go`
- `internal/pipeline/pipeline.go`
- `internal/pipeline/agent_run.go`
- `internal/pipeline/steps/common_exec.go`
- `internal/pipeline/steps/publication.go`
- `internal/daemon/publication_composition.go`
- `internal/publication/candidate_port.go`
- `cmd/no-mistakes/main.go`

Tests:

- new `internal/agent/publication_boundary_test.go`
- new `internal/agent/publication_boundary_linux_test.go`
- `internal/agent/codex_test.go`
- `internal/pipeline/agent_run_test.go`
- `internal/pipeline/steps/publication_defense_test.go`
- `internal/pipeline/steps/publication_test.go`
- `internal/daemon/publication_composition_test.go`
- `internal/publication/candidate_port_test.go`
- `cmd/no-mistakes/publication_main_test.go`

The single policy owner is `internal/agent/publication_boundary.go`; the Linux
file owns the namespace canary and bounded teardown, while the unsupported file
returns `confinement_unavailable` on every non-Linux platform. The private
self-exec canary dispatch is owned by `cmd/no-mistakes/main.go`; it is not a
public workflow command and grants no publication authority. The one reaping
call site is the boundary launch/wait implementation, not `internal/procreap`.
`internal/pipeline/steps/publication.go` suppresses candidate disposal on the
distinguished uncertain-cleanup error; `internal/publication/candidate_port.go`
retains that recoverable container and removes only after a certain boundary
return. Ordinary AXI and ordinary `internal/procreap` call sites are unchanged.

### Complete N0 historical boundary

New production owners:

- `internal/publication/protocol.go`
- `internal/publication/manager.go`
- `internal/publication/candidate.go`
- `internal/publication/effects.go`
- `internal/publication/result.go`
- `internal/db/publication.go`
- `internal/cli/publication.go`
- `internal/pipeline/steps/publication.go`

Existing production owners changed only as required:

- `internal/types/types.go`
- `internal/db/schema.go`
- `internal/db/run.go`
- `internal/pipeline/pipeline.go`
- `internal/pipeline/executor.go`
- `internal/pipeline/steps/common.go`
- `internal/pipeline/steps/review.go`
- `internal/pipeline/steps/test.go`
- `internal/pipeline/steps/document.go`
- `internal/pipeline/steps/lint.go`
- `internal/pipeline/steps/common_fix.go`
- `internal/pipeline/steps/common_exec.go`
- `internal/daemon/manager.go`
- `internal/daemon/daemon.go`
- `internal/daemon/publication_composition.go`
- `internal/ipc/protocol.go`
- `internal/cli/root.go`
- `cmd/no-mistakes/main.go`
- `internal/agent/agent.go`
- `internal/agent/codex.go`
- `internal/agent/env.go`
- `internal/scm/host.go`
- `internal/scm/github/github.go`

Documentation and test owners:

- this ExecPlan
- new `docs/src/content/docs/reference/factory-publication.md`
- `docs/src/content/docs/reference/cli.md`
- `docs/src/content/docs/concepts/pipeline.md`
- the exact active-slice test files listed above, plus the already landed N0
  tests beside their historical owners
- new offline `internal/e2e/factory_publication_test.go`

Do not change the generated skill unless the public agent-facing skill actually
needs this command. If it does, edit only `internal/skill/skill.go` and
regenerate through the repository-owned process.

## Proof obligations

- strict canonical JSON and publication-ID collision refusal;
- exact Factory, candidate, WorkContract, intent, and publisher bindings;
- concurrent identical requests create one Run; ordinary AXI Runs never attach;
- all defense steps execute, none skip, and the candidate cannot mutate;
- the model-free current-binary capability probe passes before admission and
  proves candidate-read plus scratch-write positive controls; its canonical
  policy digest and complete ordered executable-closure manifest/version/
  platform are identical to the values revalidated before every agent and
  configured-command launch;
- candidate/source/sibling/home reads or writes outside the exact grants,
  chmod/rename/link attacks, ambient credentials/proxies/askpass, TCP, and
  unallowlisted Unix sockets are denied for agent tools and configured
  commands;
- conflicting Codex args, another agent, fallback, session/resume, unsupported
  platform/build, missing sandbox support, or nonempty unsandboxed configured
  commands fail before process/admission effects;
- Linux cancellation and clean exit synchronously destroy the exact PID
  namespace containing a real `setsid` descendant after it changes CWD to `/`
  and closes inherited descriptors; a pre-exec `NSpid`-tail-1 barrier binds the
  exact namespace init, and exact absence of that init PID/start identity after
  wrapper wait proves no survivor; no late marker survives, and an unrelated
  closure-bound live sentinel is never signalled;
- any logical-entry/native/sandbox-helper/bubblewrap/profile swap between probe
  and launch, policy/argv drift between
  model-free probe, agent execution, and configured Test/Lint, an unavailable
  PID namespace, or uncertain teardown all fail before the next effect; the
  latter retains candidate/scratch evidence;
- Gate-before-effect call count is zero until the exact decision is durable;
- Push/PR decisions are single-use and non-transferable;
- draft, remote, PR-head, and CI-head drift fail closed;
- PR crash reconciliation requires exactly one exact match;
- empty/skip/partial/pending/cancelled/failed/unknown/malformed CI is not READY;
- crash injection at every decision/effect/observation boundary converges or
  becomes `EFFECT_UNKNOWN`, never a duplicate effect;
- startup recovery preserves the same contract;
- ordinary standalone AXI behavior remains unchanged;
- public commands emit one strict JSON object and correct exit status.

The repository has no mutation harness. Targeted substitutions and injected
fakes are documented as falsifiers, not as a mutation score.

## Verification ladder

Run the affected unit packages first, then:

1. `gofmt -w .`
2. `make lint`
3. `go test -race ./...`
4. `make e2e`
5. `go build -o ./bin/no-mistakes ./cmd/no-mistakes`
6. offline manual falsifiers for duplicate JSON, attach, exact-H, gate-before-
   effect, PR ambiguity, empty/skip CI, and crash replay
7. current-byte model-free Codex sandbox negative canary on macOS and required
   non-skipped Linux canaries on `linux/arm64` and `linux/amd64`, including the
   detached `setsid`/`chdir /`/closed-FD namespace teardown, swaps of every
   executable-closure member, probe/runtime-argv-drift, and configured-command
   boundary; no skip credit
8. exact-byte independent Senox review

An unavailable, skipped, partial, or merely exit-zero gate earns no credit.

## Hard abort conditions

Stop and return to the Owner if implementation requires:

- a new dependency or package extraction;
- sudo, host policy changes, a new container/runtime requirement, or a silent
  compatibility fallback for the Codex boundary;
- another SDLC/P4 state machine or standalone Publication executor;
- a change to ordinary AXI semantics;
- mutation of candidate `H`;
- a Push/PR gate after its effect;
- an unpinned publisher or incompatible daemon;
- blind replay after an ambiguous effect;
- a real network/auth/provider call in local tests;
- changes to identities, Rulesets, CI policy, deployment, or merge policy;
- a provider other than GitHub in v1;
- editing generated skill output directly.

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-08-29 | Extend the existing Run/Executor instead of creating a Publication aggregate. | Keeps one No Mistakes execution model and avoids a second SDLC state machine. |
| 2026-08-29 | Use two single-use Owner decisions, for Push and PR only. | Those are N0's mutating external effects; CI is read-only observation. |
| 2026-08-29 | Keep all nine step names but make publication-mode Rebase and defense steps non-mutating. | Preserves No Mistakes defense in depth without taking authority from Factory. |
| 2026-08-29 | Make v1 GitHub-only. | Exact PR snapshots and crash reconciliation need one fully proved provider contract. |
| 2026-08-29 | Bind PR recovery to a unique marker and exact draft digest, but do not treat the marker as trust. | Prevents adopting or duplicating an ambiguous PR after a crash. |
| 2026-08-29 | Treat Effect records as a crash journal, not a workflow authority. | External-effect recovery is orthogonal to SDLC semantics. |
| 2026-08-30 | Bind the live base ref to an exact durable base commit `B`; require `B` to be an ancestor of `H`. | Freshness must survive daemon restart without inventing a one-parent policy or consulting a mutable checkout. |
| 2026-08-30 | Refuse fork routing in publication protocol v1. | The v1 request does not bind a distinct fork repository identity, so accepting forks would weaken exact provider routing. |
| 2026-08-30 | Hold production publication admission at `confinement_unavailable`. | Same-UID permissions, prompts, environment denylists, and before/after hashes cannot prevent transient candidate mutation or direct credential/network effects. |
| 2026-08-30 | Owner selected `GO-N0-CONFINEMENT-CODEX`, restricted to mechanically proved Codex combinations. | Uses an existing installed sandbox primitive without silently broadening to other agents, platforms, dependencies, or unconfined fallbacks. |
| 2026-08-30 | Require the same boundary for agent tools and configured Test/Lint commands. | A direct command shell would bypass the defense boundary even if the model adapter were confined. |
| 2026-08-30 | Mark macOS production Publication unsupported and make the pinned Linux PID namespace the only process-lifecycle authority. | A real Mac falsifier proved a `setsid` descendant can change CWD to `/`, survive process-group teardown, and evade a CWD-based sweep. Linux Codex explicitly uses a bubblewrap PID namespace; exact-byte runtime canaries must still prove that behavior. |
| 2026-08-30 | Bind probe, agent, and configured-command launches to one immutable in-process policy/binary manifest. | A passing probe against mutable profile or binary A cannot authorize runtime B. |
| 2026-08-30 | Execute the pinned native Codex binary directly and bind the complete pre-sandbox executable closure. | Hashing only the npm/JS entry point does not bind the native Codex or lifecycle-critical bubblewrap/helper bytes it launches. |

## Durable Next Action / Recovery

The authenticated gate-context boundary, Executor-only CI terminalization,
exact-H terminalization, complete status pagination, and offline core journey
are closed. The Owner chose restricted Codex confinement; the Mac process
falsifier narrowed production support to Linux. Next: obtain re-review of the
canonical binary/policy binding and Linux namespace-teardown plan; then freeze
red tests for the managed permission profile, command/agent shared boundary,
fail-before-admission matrix, and detached-child namespace teardown; implement
without a dependency or fallback; run current-byte Mac negative plus OrbStack
and VPS Linux falsifiers; then obtain independent exact-byte Senox review.
Production remains `confinement_unavailable` until every gate passes. Do not
claim M2/M4, freeze/push as release-ready, or begin N1 earlier. If work stops,
verify the exact base and branch, inspect `git status`, and rerun the last
recorded focused tests before changing product code.
