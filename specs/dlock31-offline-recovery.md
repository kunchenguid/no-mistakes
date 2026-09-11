# DLOCK31: retained CI repair recovery

## Authorized implementation, based on current upstream

Work now proceeds on `fix/dlock31-retained-ci-repair`, created from upstream
`b43ec6367d342aae2cd5713db20e04b396f140cc` without removing any probes. This
checkout has no gate registration. Upstream is `kunchenguid/no-mistakes`; parent
must verify its contributor-fork push target before gating. No direct push is
authorized. Upstream includes command gates and PR-template changes since
v1.72.0; retain those changes rather than patching the obsolete base.

### Complete implementation design

- Resolve only a syntactically valid, same-host PR URL mismatch through two
  authenticated repository GETs. Require positive integral IDs, equal IDs and
  matching canonical full names; reject malformed responses and authentication
  failures. Keep the existing strict URL/number checks. Never change registration.
- Persist a narrow pending-CI-publication record when attestation interrupts a
  continuity-proven repair. Bind run, repository, branch, CI step, recorded head,
  reviewed head and exact retained head. Anchor that retained commit under a
  run-specific non-symbolic gate ref. The database record and ref must agree.
- On explicit CI fix re-entry, inspect this record before invoking an agent.
  Verify exact HEAD, clean worktree, same gate, unchanged recorded/reviewed heads,
  and ancestry. Then reuse existing publication/attestation/lease guards; do not
  create a commit or invoke review/fixer. Trusted revalidation policy still wins:
  if it now requires Review, leave this publication retry parked, not restarted.
- Startup uses the same verifier for the sole allowed head-mismatch exception.
  It restores the original parked gate with the recorded head unchanged. A
  missing, moved, dirty, unbound or divergent correction remains refused.
- Clear the pending binding only after successful publication. Failed retries
  preserve it and remain parked. No broad retry engine or review-budget changes.

**Legacy limitation:** the existing v1.72.0 DLOCK31 run predates this explicit
binding. A prose summary and descendant commit are not provenance. This patch
must not fabricate a binding for it. Any one-time operator-authorized adoption
of that legacy repair needs a separately specified exact-head authorization
surface and installation/handover approval; no live adoption occurs here.

Tests must drive full bound recovery and the existing publication path using
temporary state and fake transport, plus rejection controls. The old admission
sentinel is replaced by a full synthetic recovery test, not merely turned green
by weakening head equality. All runtime deployment remains parent-owned.

## Original probe objective (historical)

Reproduce the three blockers to publishing the already-created CI correction
without another fixer/review invocation or loss of the original run. This task
adds tests only; it does not implement or authorize production recovery.

## Target files

- `internal/scm/github/dlock31_rename_probe_test.go`
- `internal/pipeline/steps/dlock31_publication_probe_test.go`
- `internal/daemon/dlock31_restore_probe_test.go`

Preserve the two pre-existing untracked probe tests byte-for-byte.

## Requirements

1. FindPR accepts a canonical rename only when authenticated repository metadata
   proves the old and canonical slugs identify the same GitHub repository. A
   genuinely different repository must remain rejected.
2. Re-entering CI with a clean retained attestation-failed correction must not
   invoke an agent before retrying its guarded publication.
3. Recovery must distinguish a verified retained CI correction from arbitrary
   worktree drift; the current unconditional head-equality check blocks it.

Success for this preparation is three reproducible pre-fix red cases, passing
negative/positive controls, and no production-file or live-state mutation.

## Design

Use existing command factories, fakecli, mockAgent, temporary SQLite, and local
temporary Git repositories. No sockets, actual pushes, provider calls, credentials,
installed binaries, or real run/database reads. Git config is isolated; Go module
downloads are disabled when executing the tests. No production seam is added.

- Rename: execute Host.FindPR through githubTestCmdFactory. Supply PR23 and GET
  repository metadata fixtures for both slugs (ID 1345880885); vary only the
  canonical repository ID for the rejection control. The old implementation
  rejects the URL before asking for this available identity evidence.
- CI: model a clean committed descendant and the persisted attestation-failure
  gate. Drive CIStep.Execute with Fixing=true. A mock agent cancels execution at
  its first invocation, proving the forbidden call without running a model or
  letting a push occur. This is an ordering probe, not proof of successful retry.
- Restore: use prepareRecoveredRun, the existing recovery admission seam, with
  synthetic parked records and temporary Git history. A step-factory sentinel
  observes admission past Git identity checks; an empty step set stops before
  config fetch or agent construction. Equal-head admission is the control. This
  deliberately proves the first blocker, not end-to-end restart safety.

The real published 5b29f9c8 and retained 9c771549 roles use freshly generated
temporary commit IDs: no live objects or snapshots are imported.

## Ranked predictions from the prior investigation

1. Literal URL-path validation rejects a rename even with matching ID evidence.
2. CI only recognizes protected-path retained repairs, so attestation failure
   re-entry reaches mockAgent before any publication retry.
3. Recovery rejects any head mismatch before consulting CI gate evidence.

These are already source-backed hypotheses; the new work tests them through
execution, not source-text assertions. Production repair is intentionally deferred.

## Minimum later repair (not authorized here)

- Resolve canonical GitHub identity at the Host boundary on mismatch, verify
  host and stable repository ID, then retain strict PR number/path validation.
  Do not blindly trust PRURL or mutate repository registration to work around it.
- Record/recognize the exact retained attestation-failed repair, and route an
  explicit retry before autoFixCI through existing guarded publication. Keep
  continuity, trusted revalidation policy, attestation settlement, and leases.
  A retry that needs Review must remain parked under this no-new-review contract.
- Recovery admission must verify the retained repair's provenance/head and gate
  binding, not generally accept descendants or rewrite run.HeadSHA to pass the
  equality check. The current legacy summary alone is not sufficient authority.

Approval to implement these narrow changes is separate from approval to install
or hand over the live daemon. Full offline restoration/publication tests and a
preservation plan are prerequisites to any runtime exception. Never use make
install here: it stops and starts the daemon.

## Executed pre-fix evidence (historical)

Source: `9fcc8657e1cf3b53cc6457d7e78092c2bf506929` (installed v1.72.0
source from the prior investigation). No production changes were made.

Exact repeat command, from this isolated checkout:

```sh
rtk run 'GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local NO_MISTAKES_TELEMETRY=0 go test ./internal/scm/github ./internal/pipeline/steps ./internal/daemon -run "^TestDLOCK31" -count=2 -timeout=60s -v; rc=$?; printf "DLOCK31_RED_EXIT=%s\n" "$rc"'
```

Result: `DLOCK31_RED_EXIT=1`. Each repetition had three red leaf cases and two
passing controls; two repetitions yielded **6 red / 4 passing leaf executions**.
The enclosing table tests also report FAIL; they are not additional cases.

- `TestDLOCK31RenameIdentity/same_repository`: `same GitHub ID 1345880885
  rename rejected: parse gh pr list JSON: entry 0 invalid PR URL: URL repository
  "Main-Jammers-Incorporated/soul-swap" does not match GitHub repository
  "Main-Jammers-Incorporated/deadlock-skin-marketplace"`.
- `TestDLOCK31RenameIdentity/different_repository`: PASS both times.
- `TestDLOCK31RetainedAttestationRepairDoesNotInvokeFixer`: `fixer calls=1,
  want 0; publication attempted=false`; mock cancellation returned
  `context canceled`. Worktree stayed clean and retained/recorded heads unchanged.
- `TestDLOCK31RecoveryAdmission/retained_attestation_repair`: `recovery rejected
  before gate inspection ... error=worktree head does not match run head`.
- `TestDLOCK31RecoveryAdmission/equal_head_control`: PASS both times.

The first run (`-count=1`) had the same three failures and two passing controls.
No compilation errors or test timeouts occurred.

### Limits and exact patch owners

- GitHub metadata responses are available at the fake process boundary, but
  v1.72.0 never requests them before rejecting the alias. The passing negative
  control proves today's cross-repository rejection, not a yet-unimplemented
  ID-verification algorithm. Extend its matrix with unreadable/malformed ID and
  host mismatch when implementing that algorithm.
  Owner: `internal/scm/github/github.go:220-223,282-284`.
- CI replay starts from a synthetic persisted failure, not an actual push.
  Both agent entry and publication entry are cancellation tripwires; no agent
  or push is allowed. Success of eventual publication remains untested here.
  Owners: `internal/pipeline/steps/ci.go:257-261,335-339` and
  `internal/pipeline/steps/ci_fix.go:90-99,244-258,624-632,699-703`.
- Recovery stops at a step-factory sentinel before configuration/agent setup.
  It is not a complete recoverable snapshot: the empty step list intentionally
  refuses continuation. Do not change the equality guard simply to make this
  admission probe green. A real repair needs verified retained-head evidence
  and full restoration tests with unchanged history, plus rejection controls
  for unbound or divergent heads.
  Owner: `internal/daemon/manager.go:151-153`.

### Preservation

`git diff --exit-code` remained empty for tracked files. SHA-256 checks of both
pre-existing untracked files matched before and after test execution:

```text
cb9a8c6391667e6202f0e297d2c5240abc11b96d957223cafd460dd1946bcc88  internal/pipeline/parked_restore_probe_test.go
789115736136fccd1bd362cc271db4df18e946f3e3ff53152d54e4b6b94ae089  internal/pipeline/steps/soul23_instruction_repro_test.go
```

Only the three new tests and this spec were created. The deliberately red tests
remain isolated preparation artifacts, not a passing release candidate.

## Implemented candidate and final verification

The historical red probes have now been extended into behavioral regressions:

- `internal/scm/github/rename.go` verifies same-host repository identity through
  the existing command factory. ID equality and canonical full-name agreement
  are required. Malformed, fractional, string, missing/zero IDs, wrong names,
  authentication failure and different-host controls fail closed.
- `internal/db/retained_ci.go` and its additive schema table bind exact repair
  ownership and heads. Publication and binding cleanup are one transaction in
  `UpdateRunPublication`; a synthetic cleanup failure proves rollback.
- `internal/pipeline/retained_ci.go` owns the shared local Git/DB proof. The
  run-specific ref is non-symbolic; only a previous successfully published
  anchor can advance for a subsequent repair. Missing, moved, rewound, dirty,
  unbound and divergent state remains refused.
- `internal/pipeline/steps/ci_retained.go` retries that publication before any
  agent. Authentication, missing PR, changed trusted revalidation policy,
  unsupported provider and a branch newly designated as base all keep it parked.
  The parent's two early-return cases were separately reproduced RED against
  the initial implementation before adding their retained-only refusals.
- `prepareRecoveredRun` verifies the same binding, then runs normal config,
  step-history and agent-construction checks. It does not rewrite head metadata
  to pass the equality check. The original equal-head path remains covered.

`TestDLOCK31FullResumePublishesRetainedRepair` now drives **Executor.Resume ->
explicit CI response -> real CIStep -> real attestation rewrite -> fake leased
Git transport -> gate mirror -> atomic publication record**, with zero agent
calls and the original round/run/branch/retained commit preserved. Its temporary
monitor is cancelled at the existing poll seam after publication. Separate
daemon tests cover full admission/config restoration, not the old sentinel.

Final command (all four packages PASS, **33 leaf cases PASS, 0 failures**):

```sh
rtk run 'GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local NO_MISTAKES_TELEMETRY=0 go test -race ./internal/db ./internal/daemon ./internal/pipeline/steps ./internal/scm/github -run "TestDLOCK31" -count=1 -timeout=120s'
```

Also passed: existing `TestFindPR` tests, targeted `go vet` over these packages
plus `internal/pipeline`, generated-skill drift check, and `git diff --check`.
Both original untracked probe SHA-256 values above still match.

The retry clears stale CI readiness and claims the existing push-active marker
before publication, releasing it on every exit. Both durable and in-memory
owner/branch identity must match the retained binding; the changed-owner-branch
control is included in the final cases above.

Whole-repository `make lint` and a non-installed `/tmp/dlock31-no-mistakes-build`
build were attempted with downloads disabled. Both are **blocked**, not passed:
the upstream CLI/TUI dependencies (Cobra, Bubble Tea/Bubbles, Lipgloss, Termenv,
TOON) are absent from the local module cache. No dependency/network opt-in was
made. The full test suite was not run: existing tests include real local pushes
and the preserved external-snapshot probe, outside this task's test boundary.

During fixture development, the first full-resume attempt exposed an exec-path
mistake: `exec.LookPath` preceded the environment overlay and ran `gh auth status`
under the empty temporary HOME, returning not logged in. The fixture now pins
the process lookup to the existing fakecli directory as well as setting the
run overlay. The final passing runs use fake forge/transport processes; no live
push, model call, daemon control, installed-binary or SoulSwap mutation occurred.

Publication target checked read-only: `mwaddo/no-mistakes` is a push-authorized
fork of `kunchenguid/no-mistakes`, base `main`. This checkout remains unregistered
with only its original origin remote. Parent owns fork/gate setup, review and
publication. No public issue/PR was posted.

**Remaining live-run boundary:** this candidate safely recovers explicitly bound
repairs. It deliberately does not retroactively bless the unbound v1.72.0
SoulSwap repair. That run still requires an explicit, exact-head legacy-adoption
design/authorization and a separately approved deployment/handover. Do not use
this candidate's passing synthetic tests as permission to restart the live daemon.
